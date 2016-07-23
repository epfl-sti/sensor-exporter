package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	_ "net/http/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/ncabatoff/gosensors"
	"github.com/prometheus/client_golang/prometheus"
)

var (
	fanspeed = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "sensor",
		Subsystem: "lm",
		Name:      "fan_speed_rpm",
		Help:      "fan speed (rotations per minute).",
	}, []string{"fantype", "chip", "adaptor"})

	voltages = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "sensor",
		Subsystem: "lm",
		Name:      "voltage_volts",
		Help:      "voltage in volts",
	}, []string{"intype", "chip", "adaptor"})

	powers = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "sensor",
		Subsystem: "lm",
		Name:      "power_watts",
		Help:      "power in watts",
	}, []string{"powertype", "chip", "adaptor"})

	temperature = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "sensor",
		Subsystem: "lm",
		Name:      "temperature_celsius",
		Help:      "temperature in celsius",
	}, []string{"temptype", "chip", "adaptor"})

	hddTempDesc = prometheus.NewDesc(
		"sensor_hddsmart_temperature_celsius",
		"temperature in celsius",
		[]string{"device", "id"},
		nil)
)

func init() {
	prometheus.MustRegister(fanspeed)
	prometheus.MustRegister(voltages)
	prometheus.MustRegister(powers)
	prometheus.MustRegister(temperature)
}

func main() {
	var (
		listenAddress  = flag.String("web.listen-address", ":9255", "Address on which to expose metrics and web interface.")
		metricsPath    = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics.")
		hddtempAddress = flag.String("hddtemp-address", "localhost:7634", "Address to fetch hdd metrics from.")
	)
	flag.Parse()

	prometheus.EnableCollectChecks(true)
	hddcollector := NewHddCollector(*hddtempAddress)
	if err := hddcollector.Init(); err != nil {
		log.Printf("error readding hddtemps: %v", err)
	}
	prometheus.MustRegister(hddcollector)

	go collectLm()

	http.Handle(*metricsPath, prometheus.Handler())

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
			<head><title>Sensor Exporter</title></head>
			<body>
			<h1>Sensor Exporter</h1>
			<p><a href="` + *metricsPath + `">Metrics</a></p>
			</body>
			</html>`))
	})
	http.ListenAndServe(*listenAddress, nil)
}

func collectLm() {
	gosensors.Init()
	defer gosensors.Cleanup()
	for {
		for _, chip := range gosensors.GetDetectedChips() {
			chipName := chip.String()
			adaptorName := chip.AdapterName()
			for _, feature := range chip.GetFeatures() {
				if strings.HasPrefix(feature.Name, "fan") {
					fanspeed.WithLabelValues(feature.GetLabel(), chipName, adaptorName).Set(feature.GetValue())
				} else if strings.HasPrefix(feature.Name, "temp") {
					temperature.WithLabelValues(feature.GetLabel(), chipName, adaptorName).Set(feature.GetValue())
				} else if strings.HasPrefix(feature.Name, "in") {
					voltages.WithLabelValues(feature.GetLabel(), chipName, adaptorName).Set(feature.GetValue())
				} else if strings.HasPrefix(feature.Name, "power") {
					powers.WithLabelValues(feature.GetLabel(), chipName, adaptorName).Set(feature.GetValue())
				}
			}

		}
		time.Sleep(1 * time.Second)
	}
}

type (
	HddCollector struct {
		address string
		conn    net.Conn
		buf     bytes.Buffer
	}

	HddTemperature struct {
		Device             string
		Id                 string
		TemperatureCelsius float64
	}
)

func NewHddCollector(address string) *HddCollector {
	return &HddCollector{
		address: address,
	}
}

func (h *HddCollector) Init() error {
	conn, err := net.Dial("tcp", h.address)
	if err != nil {
		return fmt.Errorf("error connecting to hddtemp address '%s': %v", h.address, err)
	}
	h.conn = conn
	return nil
}

func (h *HddCollector) readTempsFromConn() (string, error) {
	_, err := io.Copy(&h.buf, h.conn)
	if err != nil {
		return "", fmt.Errorf("Error reading from hddtemp socket: %v", err)
	}
	return h.buf.String(), nil
}

func (h *HddCollector) Close() error {
	if err := h.conn.Close(); err != nil {
		return fmt.Errorf("Error closing hddtemp socket: %v", err)
	}
	return nil
}

func parseHddTemps(s string) ([]HddTemperature, error) {
	var hddtemps []HddTemperature
	if len(s) < 1 || s[0] != '|' {
		return nil, fmt.Errorf("Error parsing output from hddtemp: %s", s)
	}
	for _, item := range strings.Split(s[1:len(s)-1], "||") {
		hddtemp, err := parseHddTemp(item)
		if err != nil {
			return nil, fmt.Errorf("Error parsing output from hddtemp: %v", err)
		} else {
			hddtemps = append(hddtemps, hddtemp)
		}
	}
	return hddtemps, nil
}

func parseHddTemp(s string) (HddTemperature, error) {
	pieces := strings.Split(s, "|")
	if len(pieces) != 4 {
		return HddTemperature{}, fmt.Errorf("error parsing item from hddtemp, expected 4 tokens: %s", s)
	}
	if pieces[3] != "C" {
		return HddTemperature{}, fmt.Errorf("error parsing item from hddtemp, I only speak Celsius", s)
	}

	dev, id, temp := pieces[0], pieces[1], pieces[2]
	ftemp, err := strconv.ParseFloat(temp, 64)
	if err != nil {
		return HddTemperature{}, fmt.Errorf("Error parsing temperature as float: %s", temp)
	}

	return HddTemperature{Device: dev, Id: id, TemperatureCelsius: ftemp}, nil
}

// Describe implements prometheus.Collector.
func (e *HddCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- hddTempDesc
}

// Collect implements prometheus.Collector.
func (h *HddCollector) Collect(ch chan<- prometheus.Metric) {
	tempsString, err := h.readTempsFromConn()
	if err != nil {
		log.Printf("error reading temps from hddtemp daemon: %v", err)
		return
	}
	hddtemps, err := parseHddTemps(tempsString)
	if err != nil {
		log.Printf("error parsing temps from hddtemp daemon: %v", err)
		return
	}

	for _, ht := range hddtemps {
		ch <- prometheus.MustNewConstMetric(hddTempDesc,
			prometheus.GaugeValue,
			ht.TemperatureCelsius,
			ht.Device,
			ht.Id)
	}
}
