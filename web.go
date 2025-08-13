package main

import (
	"fmt"
	ds "github.com/starfederation/datastar-go/datastar"
	"net/http"
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
	Data        []GraphData
}

var charts = []chartProps{
	{"TPS", "Throotle", nil},
	{"RPM", "Revvie wevvy", nil},
}

func buildUpdateChartScript(chartName string, x, y int) string {
	return fmt.Sprintf(`pushData(%s, %d, %d);`, fmt.Sprintf("%sBuffer", chartName), x, y)
}

func IndexHandler(w http.ResponseWriter, _ *http.Request) {
	err := Templates.ExecuteTemplate(w, "index", map[string]interface{}{
		"cards": cards,
		"tpsChartProps": chartProps{
			Name:        "TPS",
			Description: "Throttle Position Sensor",
			Data:        tpsHistory,
		},
		"rpmChartProps": chartProps{
			Name:        "RPM",
			Description: "Revolutions Per Minute",
			Data:        rpmHistory,
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
