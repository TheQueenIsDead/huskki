package main

import (
	"fmt"
	ds "github.com/starfederation/datastar-go/datastar"
	"net/http"
	"strings"
)

type cardProps struct {
	Name  string
	Value any
	Unit  string
}

var cards = []cardProps{
	{"Throttle", 0, "%"},
	{"Grip", 0, "%"},
	{"TPS", 0, "%"},
	{"RPM", 0, "RPM"},
	{"Coolant", 0, "°C"},
}

type chartProps struct {
	Name        string
	Description string
	Labels      []int
	Data        []int
}

var charts = []chartProps{
	{"TPS", "Throotle", nil, nil},
	{"RPM", "Revvie wevvy", nil, nil},
}

func buildUpdateChartScript(name string, label, data int) string {
	return fmt.Sprintf(`(function(){
		let chart = Chart.getChart("%s-chart");
		chart.data.labels.push(%d);
		chart.data.datasets[0].data.push(%d);

		if (chart.data.labels.length > 20) {
			chart.data.labels.shift();
			chart.data.datasets[0].data.shift();
		}
		chart.update();
	})()`, strings.ToLower(name), label, data)
}

func IndexHandler(w http.ResponseWriter, _ *http.Request) {
	err := Templates.ExecuteTemplate(w, "index", map[string]interface{}{
		"cards": cards,
		"tpsChartProps": chartProps{
			Name:        "TPS",
			Description: "Throttle Position Sensor",
			Labels:      tpsHistoryLabels,
			Data:        tpsHistoryData,
		},
		"rpmChartProps": chartProps{
			Name:        "RPM",
			Description: "Revolutions Per Minute",
			Labels:      rpmHistoryLabels,
			Data:        rpmHistoryData,
		},
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func EventsHandler(w http.ResponseWriter, r *http.Request) {
	sse := ds.NewSSE(w, r)

	_, ch, cancel := EventHub.Subscribe()
	defer cancel()

	for {
		select {
		case <-r.Context().Done():
			return
		case event := <-ch:
			updateFunc := generatePatch(event)
			err := updateFunc(sse)
			if err != nil {
				fmt.Println(err)
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
		}
	}
}
