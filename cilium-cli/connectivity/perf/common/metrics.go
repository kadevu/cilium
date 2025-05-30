// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package common

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"time"
)

// LatencyMetric captures latency metrics of network performance test
type LatencyMetric struct {
	Min    time.Duration `json:"Min"`
	Avg    time.Duration `json:"Avg"`
	Max    time.Duration `json:"Max"`
	Perc50 time.Duration `json:"Perc50"`
	Perc90 time.Duration `json:"Perc90"`
	Perc99 time.Duration `json:"Perc99"`
}

// toPerfData export LatencyMetric in a format compatible with perfdash scheme
func (metric *LatencyMetric) toPerfData(labels map[string]string, prefix string) dataItem {
	resLabels := map[string]string{
		"metric": "Latency",
	}
	maps.Copy(resLabels, labels)
	return dataItem{
		Data: map[string]float64{
			// Let's only export percentiles
			// Max is skewing results and doesn't make much sense to keep track of
			prefix + "_p50": float64(metric.Perc50) / float64(time.Microsecond),
			prefix + "_p90": float64(metric.Perc90) / float64(time.Microsecond),
			prefix + "_p99": float64(metric.Perc99) / float64(time.Microsecond),
		},
		Unit:   "us",
		Labels: resLabels,
	}
}

// TransactionRateMetric captures transaction rate metric of network performance test
type TransactionRateMetric struct {
	TransactionRate float64 `json:"Rate"` // Ops per second
}

// ToPerfData export TransactionRateMetric in a format compatible with perfdash scheme
func (metric *TransactionRateMetric) toPerfData(labels map[string]string, prefix string) dataItem {
	resLabels := map[string]string{
		"metric": "TransactionRate",
	}
	maps.Copy(resLabels, labels)
	return dataItem{
		Data: map[string]float64{
			prefix + "_throughput": metric.TransactionRate,
		},
		Unit:   "ops/s",
		Labels: resLabels,
	}
}

// ThroughputMetric captures throughput metric of network performance test
type ThroughputMetric struct {
	Throughput float64 `json:"Throughput"` // Throughput in bytes/s
}

// ToPerfData export ThroughputMetric in a format compatible with perfdash scheme
func (metric *ThroughputMetric) toPerfData(labels map[string]string, prefix string) dataItem {
	resLabels := map[string]string{
		"metric": "Throughput",
	}
	maps.Copy(resLabels, labels)
	return dataItem{
		Data: map[string]float64{
			prefix + "_throughput": metric.Throughput / 1000000,
		},
		Unit:   "Mb/s",
		Labels: resLabels,
	}
}

// PerfResult stores information about single network performance test results
type PerfResult struct {
	Timestamp             time.Time
	Latency               *LatencyMetric
	TransactionRateMetric *TransactionRateMetric
	ThroughputMetric      *ThroughputMetric
}

// PerfTests stores metadata information about performed test
type PerfTests struct {
	Tool     string
	Test     string
	SameNode bool
	Scenario string
	Sample   int
	MsgSize  int
	Duration time.Duration
	Streams  uint
	NetQos   bool
}

// PerfSummary stores combined metadata information and results of test
type PerfSummary struct {
	PerfTest PerfTests
	Result   PerfResult
}

// These two structures are borrowed from kubernetes/perf-tests:
// https://github.com/kubernetes/perf-tests/blob/master/clusterloader2/pkg/measurement/util/perftype.go
// this is done in order to be compatible with perfdash
type dataItem struct {
	// Data is a map from bucket to real data point (e.g. "Perc90" -> 23.5). Notice
	// that all data items with the same label combination should have the same buckets.
	Data map[string]float64 `json:"data"`
	// Unit is the data unit. Notice that all data items with the same label combination
	// should have the same unit.
	Unit string `json:"unit"`
	// Labels is the labels of the data item.
	Labels map[string]string `json:"labels,omitempty"`
}

// PerfData contains all data items generated in current test.
type perfData struct {
	// Version is the version of the metrics. The metrics consumer could use the version
	// to detect metrics version change and decide what version to support.
	Version   string     `json:"version"`
	DataItems []dataItem `json:"dataItems"`
	// Labels is the labels of the dataset.
	Labels map[string]string `json:"labels,omitempty"`
}

func getLabelsForTest(summary PerfSummary) map[string]string {
	node := "other-node"
	if summary.PerfTest.SameNode {
		node = "same-node"
	}
	return map[string]string{
		"node":      node,
		"test_type": summary.PerfTest.Tool,
	}
}

// ExportPerfSummaries exports Perfsummary in a format compatible with perfdash
// and saves results in reportDir directory
func ExportPerfSummaries(summaries []PerfSummary, reportDir string) error {
	data := map[string]dataItem{}
	for _, summary := range summaries {
		labels := getLabelsForTest(summary)
		identifier := fmt.Sprintf("%s-%s", labels["node"], labels["test_type"])
		if summary.Result.Latency != nil {
			res := summary.Result.Latency.toPerfData(labels, summary.PerfTest.Test+"_"+summary.PerfTest.Scenario)
			if _, ok := data[identifier+"lat"]; !ok {
				data[identifier+"lat"] = res
			} else {
				maps.Copy(data[identifier+"lat"].Data, res.Data)
			}

		}
		if summary.Result.TransactionRateMetric != nil {
			res := summary.Result.TransactionRateMetric.toPerfData(labels, summary.PerfTest.Test+"_"+summary.PerfTest.Scenario)
			if _, ok := data[identifier+"tr"]; !ok {
				data[identifier+"tr"] = res
			} else {
				maps.Copy(data[identifier+"tr"].Data, res.Data)
			}

		}
		if summary.Result.ThroughputMetric != nil {
			res := summary.Result.ThroughputMetric.toPerfData(labels, summary.PerfTest.Test+"_"+summary.PerfTest.Scenario)
			if _, ok := data[identifier+"th"]; !ok {
				data[identifier+"th"] = res
			} else {
				maps.Copy(data[identifier+"th"].Data, res.Data)
			}
		}
	}
	return exportSummary(perfData{Version: "v1", DataItems: slices.Collect(maps.Values(data))}, reportDir)
}

func exportSummary(content perfData, reportDir string) error {
	// this filename needs to be in a specific format for perfdash
	fileName := strings.Join([]string{"NetworkPerformance_benchmark", time.Now().Format(time.RFC3339)}, "_")
	filePath := path.Join(reportDir, strings.Join([]string{fileName, "json"}, "."))
	contentStr, err := prettyPrintJSON(content)
	if err != nil {
		return fmt.Errorf("error formatting summary: %v error: %w", content, err)
	}
	if err := os.WriteFile(filePath, []byte(contentStr), 0600); err != nil {
		return fmt.Errorf("writing to file %v error: %w", filePath, err)
	}
	return nil
}

func prettyPrintJSON(data any) (string, error) {
	output := &bytes.Buffer{}
	if err := json.NewEncoder(output).Encode(data); err != nil {
		return "", fmt.Errorf("building encoder error: %w", err)
	}
	formatted := &bytes.Buffer{}
	if err := json.Indent(formatted, output.Bytes(), "", "  "); err != nil {
		return "", fmt.Errorf("indenting error: %w", err)
	}
	return formatted.String(), nil
}
