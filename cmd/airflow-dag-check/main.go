package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/sensu-community/sensu-plugin-sdk/sensu"
	"github.com/sensu/sensu-go/types"
)

// Config represents the check plugin config.
type Config struct {
	sensu.PluginConfig
	AirflowApiUrl   string
	AirflowUsername string
	AirflowPassword string
	Dags            []string
	Timeout         int
}

var (
	plugin = Config{
		PluginConfig: sensu.PluginConfig{
			Name:     "sensu-airflow-check",
			Short:    "A plugin for checking the health of airflow 2.0 dag runs in Sensu.",
			Keyspace: "sensu.io/plugins/airflow-dag-check/config",
		},
	}

	options = []*sensu.PluginConfigOption{
		{
			Path:      "airflow-api-url",
			Env:       "",
			Argument:  "url",
			Shorthand: "url",
			Default:   "https://127.0.0.1:8081/",
			Usage:     "The base URL of the airflow REST API.",
			Value:     &plugin.AirflowApiUrl,
		},
		{
			Path:      "airflow-username",
			Env:       "",
			Argument:  "username",
			Shorthand: "un",
			Default:   "",
			Usage:     "The username used to authenticate against the airflow API.",
			Value:     &plugin.AirflowUsername,
		},
		{
			Path:      "airflow-password",
			Env:       "",
			Argument:  "password",
			Shorthand: "pw",
			Default:   "",
			Usage:     "The password used to authenticate against the airflow API.",
			Value:     &plugin.AirflowPassword,
		},
		{
			Path:      "dag",
			Env:       "",
			Argument:  "dag",
			Shorthand: "d",
			Default:   []string{},
			Usage:     "Explicit list of dags to check.",
			Value:     &plugin.Dags,
		},
		{
			Path:      "timeout",
			Env:       "",
			Argument:  "timeout",
			Shorthand: "to",
			Default:   15,
			Usage:     "Request timeout in seconds",
			Value:     &plugin.Timeout,
		},
	}
)

func main() {
	check := sensu.NewGoCheck(&plugin.PluginConfig, options, checkArgs, executeCheck, false)
	check.Execute()
}

func checkArgs(event *types.Event) (int, error) {
	_, err := url.Parse(plugin.AirflowApiUrl)
	if err != nil {
		return sensu.CheckStateWarning, fmt.Errorf("failed to parse airflow URL %s: %v", plugin.AirflowApiUrl, err)
	}

	if plugin.AirflowUsername == "" {
		return sensu.CheckStateWarning, fmt.Errorf("airflow username is required")
	}

	if plugin.AirflowPassword == "" {
		return sensu.CheckStateWarning, fmt.Errorf("airflow password is required")
	}

	return sensu.CheckStateOK, nil
}

func executeCheck(event *types.Event) (int, error) {
	client := http.DefaultClient
	client.Transport = http.DefaultTransport
	client.Timeout = time.Duration(plugin.Timeout) * time.Second

	var err error
	explicit := true
	dags := plugin.Dags

	if len(dags) == 0 {
		explicit = false
		var dagList *DagList
		dagList, err = getAllDags(client)
		if err != nil {
			return sensu.CheckStateCritical, fmt.Errorf("could not retrieve DAGs: %v", err)
		} else {
			dags = make([]string, dagList.TotalEntries)
			for i, d := range dagList.Dags {
				dags[i] = d.DagId
			}
		}
	}

	health := checkDags(dags, explicit, client)

	oks := 0
	warnings := 0
	criticals := 0
	unknowns := 0
	found := false

	for _, h := range health {
		found = true
		switch h.Status {
		case sensu.CheckStateOK:
			oks++
		case sensu.CheckStateWarning:
			warnings++
			fmt.Printf("%s WARNING\n", h.DagId)
		case sensu.CheckStateCritical:
			criticals++
			fmt.Printf("%s CRITICAL\n", h.DagId)
		case sensu.CheckStateUnknown:
			unknowns++
			fmt.Printf("%s UNKNOWN\n", h.DagId)
		}

		if h.Error != nil {
			fmt.Printf("Error occurred while checking DAG:\n%v\n", h.Error)
		}
	}

	if criticals > 0 || unknowns > 0 {
		return sensu.CheckStateCritical, nil
	} else if warnings > 0 {
		return sensu.CheckStateWarning, nil
	}

	if found {
		fmt.Printf("All health checks returning OK for loaded DAGs")
	} else {
		fmt.Printf("No DAGS loaded")
	}

	return sensu.CheckStateOK, nil
}

type Health struct {
	DagId  string
	Status int
	Error  error
}

func checkDags(dags []string, explicit bool, client *http.Client) []Health {
	var result []Health

	for _, dagId := range dags {
		var health Health
		health.DagId = dagId
		health.Status = sensu.CheckStateOK

		var err error
		var dag *Dag
		dag, err = getDag(dagId, client)

		if dag == nil {
			health.Error = fmt.Errorf("could not retrieve dag: %s\n%v", dagId, err)
			health.Status = sensu.CheckStateCritical
		} else if explicit && dag.IsPaused {
			health.Error = fmt.Errorf("DAG is paused and will not process: %s", dagId)
			health.Status = sensu.CheckStateWarning
		} else {
			var dagRun *DagRun
			dagRun, err = getLatestDagRun(dagId, client)
			health.Error = err

			// if the dag has not run, ignore it
			if dagRun != nil && dagRun.State == "failed" {
				health.Error = fmt.Errorf("DAG failed its last execution: %s", dagId)
				health.Status = sensu.CheckStateCritical
			}
		}

		result = append(result, health)
	}

	return result
}

type Dag struct {
	DagId    string `json:"dag_id"`
	IsPaused bool   `json:"is_paused"`
}

func getDag(dagId string, client *http.Client) (*Dag, error) {
	req, err := http.NewRequest("GET", getAirflowApiUrl()+"/dags/"+dagId, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(plugin.AirflowUsername, plugin.AirflowPassword)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	var result Dag
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode dag response: %v", err)
	}

	return &result, nil
}

type DagList struct {
	Dags         []Dag `json:"dags"`
	TotalEntries int   `json:"total_entries"`
}

func getAllDags(client *http.Client) (*DagList, error) {
	req, err := http.NewRequest("GET", getAirflowApiUrl()+"/dags", nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(plugin.AirflowUsername, plugin.AirflowPassword)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	var result DagList
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode dag list response: %v", err)
	}

	return &result, nil
}

type DagRun struct {
	State string `json:"state"`
}

type DagRunList struct {
	DagRuns      []DagRun `json:"dag_runs"`
	TotalEntries int      `json:"total_entries"`
}

func getLatestDagRun(dagId string, client *http.Client) (*DagRun, error) {
	req, err := http.NewRequest("GET", getAirflowApiUrl()+"/dags/"+dagId+"/dagRuns?limit=1", nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(plugin.AirflowUsername, plugin.AirflowPassword)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	var result DagRunList
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to decode dag run list response: %v", err)
	}

	if len(result.DagRuns) == 0 {
		return nil, nil
	}

	return &result.DagRuns[0], nil
}

func getAirflowApiUrl() string {
	// a trailing slash will cause errors
	return strings.TrimSuffix(plugin.AirflowApiUrl, "/") + "/api/v1"
}