package main

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/promlog"
	"github.com/prometheus/common/version"
	"github.com/prometheus/exporter-toolkit/web"
	webflag "github.com/prometheus/exporter-toolkit/web/kingpinflag"
)

const (
	exporter     = "areca_exporter"
	default_port = 9423
)

func runArecaCli(cmd string) []byte {
	out, err := exec.Command("areca.cli64", cmd).Output()

	if err != nil {
		level.Error(logger).Log("err", err)
	}

	return out
}

func getSysInfo() prometheus.Labels {
	out := runArecaCli("sys info")

	// split by newline, look for ": " and split by that
	// then trim the space from the key and value
	// then add to map
	m := make(map[string]string)
	for _, line := range bytes.Split(out, []byte("\n")) {
		if bytes.Contains(line, []byte(": ")) {
			kv := bytes.Split(line, []byte(": "))

			// convert key and to lowercase and replace spaces with underscores
			// this is to make it more prometheus friendly
			key := string(bytes.TrimSpace(kv[0]))
			key = strings.ReplaceAll(key, " ", "_")
			key = strings.ToLower(key)

			// skip if key is guierrmsg<0x00>
			if strings.HasPrefix(key, "guierrmsg") {
				continue
			}

			m[key] = string(bytes.TrimSpace(kv[1]))
		}
	}

	return prometheus.Labels(m)
}

func getRaidSetInfo() []map[string]string {
	out := runArecaCli("rsf info")

	// create array of raid sets
	var raidSets []map[string]string

	for _, line := range bytes.Split(out, []byte("\n")) {
		// skip lines we don't care about
		if len(line) == 0 || !(line[1] >= '0' && line[1] <= '9') {
			continue
		}

		// remove all spaces and create array with just the non-space elements
		var raidSet []string
		for _, kv := range bytes.Split(line, []byte(" ")) {
			if len(kv) != 0 && !(bytes.Contains(kv, []byte("Raid")) || bytes.Contains(kv, []byte("Set")) || bytes.Contains(kv, []byte("#"))) {
				raidSet = append(raidSet, string(kv))
			}
		}

		// add to hashmap
		m := make(map[string]string)

		m["id"] = raidSet[0]
		m["name"] = "Raid Set ## " + raidSet[1]
		m["disks"] = raidSet[2]
		m["total_capacity"] = raidSet[3]
		m["free_capacity"] = raidSet[4]
		m["disk_channels"] = raidSet[5]
		m["state"] = raidSet[6]

		raidSets = append(raidSets, m)
	}

	return raidSets
}

func recordMetrics() {
	arecaSysInfo.Set(1)

	// create all raid set metrics initially
	metrics := getRaidSetInfo()
	var raidSetGauges []prometheus.Gauge

	// create new gauge for each raid set
	for _, m := range metrics {
		raidSet := promauto.NewGauge(prometheus.GaugeOpts{
			Name:        "areca_raid_set_state",
			Help:        "Areca raid set state, 0 for normal, 1 for degraded",
			ConstLabels: prometheus.Labels(m),
		})
		if m["state"] == "Normal" {
			raidSet.Set(0)
		} else {
			raidSet.Set(1)
		}
		raidSetGauges = append(raidSetGauges, raidSet)
	}

	go func() {
		for {
			// update raid set metrics
			metrics := getRaidSetInfo()

			for i, m := range metrics {
				if m["state"] == "Normal" {
					raidSetGauges[i].Set(0)
				} else {
					raidSetGauges[i].Set(1)
				}
			}

			time.Sleep(5 * time.Second)
		}
	}()
}

var (
	logger = promlog.New(&promlog.Config{})

	arecaSysInfo = promauto.NewGauge(prometheus.GaugeOpts{
		Name:        "areca_sys_info",
		Help:        "Constant metric with value 1 labeled with info about Areca controller.",
		ConstLabels: getSysInfo(),
	})
)

func main() {
	toolkitFlags := webflag.AddFlags(kingpin.CommandLine, ":"+fmt.Sprint(default_port))

	kingpin.Version(version.Print(exporter))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	recordMetrics()

	level.Info(logger).Log("msg", "Starting areca_exporter", "version", version.Info())
	level.Info(logger).Log("msg", "Build context", "build_context", version.BuildContext())

	http.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{}
	if err := web.ListenAndServe(srv, toolkitFlags, logger); err != nil {
		level.Error(logger).Log("err", err)
		os.Exit(1)
	}
}
