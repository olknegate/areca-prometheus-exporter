package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime/debug"
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

func runArecaCli(cmd string) ([]byte, error) {
	ctx := context.Background()

	var cancel context.CancelFunc
	ctx, cancel = context.WithTimeout(context.Background(), time.Duration(60)*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, *cliPath, cmd).Output()

	if err != nil {
		level.Error(logger).Log("err", err, "msg", out)
	}

	return out, err
}

func getSysInfo() prometheus.Labels {
	// parse arguments early because getSysInfo might not be called before the first parsing of cli arguments
	kingpin.Parse()

	out, cmd_err := runArecaCli("sys info")

	if cmd_err != nil {
		arecaSysInfoUp.Set(1)
		return nil
	}

	defer func() {
		if panicInfo := recover(); panicInfo != nil {
			level.Error(logger).Log("err", panicInfo, "msg", debug.Stack())
			arecaSysInfoUp.Set(1)
		}
	}()

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

	arecaDiskInfoUp.Set(0)

	return prometheus.Labels(m)
}

func getRaidSetInfo() []map[string]string {
	out, cmd_err := runArecaCli("rsf info")

	if cmd_err != nil {
		arecaRsfInfoUp.Set(1)
		return nil
	}

	defer func() {
		if panicInfo := recover(); panicInfo != nil {
			level.Error(logger).Log("err", panicInfo, "msg", debug.Stack())
			arecaRsfInfoUp.Set(1)
		}
	}()

	// create array of raid sets
	var raidSets []map[string]string

	// recognize first line key names
	header_line := string(bytes.Split(out, []byte("\n"))[0])

	// split header by space, turn each element into lowercase and put into array
	var headerKeys []string
	for _, key := range strings.Split(header_line, " ") {
		// ignore empthy
		if len(key) == 0 {
			continue
		}
		key = strings.ToLower(key)
		// replace invalid label char with valid metric
		if key == "#" {
			key = "num"
		}
		headerKeys = append(headerKeys, string(key))
	}

	// then iterate over each rsf line
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

		for i, key := range headerKeys {
			if key == "name" {
				m[key] = "Raid Set # " + raidSet[i]
			} else {
				m[key] = raidSet[i]
			}
		}

		raidSets = append(raidSets, m)
	}

	arecaRsfInfoUp.Set(0)

	return raidSets
}

func getDiskInfo() []map[string]string {
	out, cmd_err := runArecaCli("disk info")

	if cmd_err != nil {
		arecaDiskInfoUp.Set(1)
		return nil
	}

	defer func() {
		if panicInfo := recover(); panicInfo != nil {
			level.Error(logger).Log("err", panicInfo, "msg", debug.Stack())
			arecaDiskInfoUp.Set(1)
		}
	}()

	// create array of raid sets
	var disks []map[string]string

	// recognize first line key names
	header_line := string(bytes.Split(out, []byte("\n"))[0])

	// split header by space, turn each element into lowercase and put into array
	var headerKeys []string
	for _, key := range strings.Split(header_line, " ") {
		// ignore empthy
		if len(key) == 0 {
			continue
		}
		key = strings.ToLower(key)

		if key == "#" {
			key = "num"
		}
		// if key contains # but not == #, then strip the hashtag
		if strings.Contains(key, "#") && key != "#" {
			key = strings.ReplaceAll(key, "#", "")
		}

		headerKeys = append(headerKeys, string(key))
	}

	// then iterate over each disk line, start from line 2 and end at the third to last
	for _, line := range bytes.Split(out, []byte("\n"))[2 : len(bytes.Split(out, []byte("\n")))-3] {
		var disk []string
		for _, kv := range bytes.Split(line, []byte("  ")) {
			if len(kv) != 0 {
				// add to disk, strip all empty spaces
				disk = append(disk, string(bytes.TrimSpace(kv)))
			}
		}

		// add to hashmap
		m := make(map[string]string)

		for i, key := range headerKeys {
			m[key] = disk[i]
		}

		disks = append(disks, m)
	}

	arecaDiskInfoUp.Set(0)

	return disks
}

func regRsfMetric(rsf_info map[string]string) prometheus.Gauge {
	raidSet := promauto.NewGauge(prometheus.GaugeOpts{
		Name:        "areca_raid_set_state",
		Help:        "Areca raid set state, 0 for normal, 1 for degraded",
		ConstLabels: prometheus.Labels(rsf_info),
	})
	if rsf_info["state"] == "Normal" {
		raidSet.Set(0)
	} else {
		raidSet.Set(1)
	}
	return raidSet
}

func recordMetrics() {
	arecaSysInfo.Set(1)
	arecaRsfInfoUp.Set(0)
	arecaDiskInfoUp.Set(0)

	// create new gauge for each raid set, and each disk
	var raidSetGauges []prometheus.Gauge
	var diskGauges []prometheus.Gauge

	// create new gauge for each raid set
	go func() {
		for {
			// get new raid set info
			rsf_info := getRaidSetInfo()

			// get new disk info
			disk_info := getDiskInfo()

			// if same amount of raid sets, then just update the labels if changed
			if len(raidSetGauges) == len(rsf_info) {
				for i, g := range raidSetGauges {
					rsf_desc := prometheus.NewDesc("areca_raid_set_state", "Areca raid set state, 0 for normal, 1 for degraded", nil, prometheus.Labels(rsf_info[i]))
					if rsf_desc != g.Desc() {
						prometheus.Unregister(g)
						raidSetGauges[i] = regRsfMetric(rsf_info[i])
					}
				}
			} else {
				// unregister all and re-register all
				for _, g := range raidSetGauges {
					prometheus.Unregister(g)
				}
				raidSetGauges = nil
				for _, m := range rsf_info {
					raidSetGauges = append(raidSetGauges, regRsfMetric(m))
				}
			}

			for _, g := range diskGauges {
				prometheus.Unregister(g)
			}

			for _, m := range disk_info {
				disk := promauto.NewGauge(prometheus.GaugeOpts{
					Name:        "areca_disk_info",
					Help:        "Constant metric with value 1 labeled with info about all physical disks attached to the Areca controller.",
					ConstLabels: prometheus.Labels(m),
				})
				disk.Set(1)
				diskGauges = append(diskGauges, disk)
			}

			time.Sleep(*collectInterval)
		}
	}()

}

var (
	logger          = promlog.New(&promlog.Config{})
	collectInterval = kingpin.Flag("collect-interval", "How often to poll Areca CLI").Default("5s").Duration()
	cliPath         = kingpin.Flag("cli-path", "Path to the Areca CLI binary").Default("areca.cli64").String()

	arecaSysInfo = promauto.NewGauge(prometheus.GaugeOpts{
		Name:        "areca_sys_info",
		Help:        "Constant metric with value 1 labeled with info about Areca controller.",
		ConstLabels: getSysInfo(),
	})
	arecaSysInfoUp = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "areca_up",
		Help: "'0' if a scrape of the Areca CLI was successful, '1' otherwise.",
		ConstLabels: prometheus.Labels{
			"collector": "sys_info",
		},
	})
	arecaRsfInfoUp = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "areca_up",
		Help: "'0' if a scrape of the Areca CLI was successful, '1' otherwise.",
		ConstLabels: prometheus.Labels{
			"collector": "rsf_info",
		},
	})
	arecaDiskInfoUp = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "areca_up",
		Help: "'0' if a scrape of the Areca CLI was successful, '1' otherwise.",
		ConstLabels: prometheus.Labels{
			"collector": "disk_info",
		},
	})
)

func main() {
	toolkitFlags := webflag.AddFlags(kingpin.CommandLine, ":"+fmt.Sprint(default_port))

	kingpin.Version(version.Print(exporter))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	prometheus.Register(version.NewCollector(exporter))

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
