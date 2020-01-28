package main

import (
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/navcanada/pgpool2-exporter/pgpool2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/version"
	"github.com/sirupsen/logrus"
)

var (
	showVersion   = flag.Bool("version", false, "Prints version information and exit")
	metricsPath   = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics.")
	listenAddress = flag.String("web.listen-address", ":9288", "Address on which to expose metrics and web interface.")
	pcpPassFile   = flag.String("pcp.passfile", "", "Path to the PCP password file containing hostname:port:username:password")
	pcpHostname   = flag.String("pcp.host", "127.0.0.1", "PCP hostname")
	pcpPort       = flag.Int("pcp.port", 9898, "PCP port")
	pcpUsername   = flag.String("pcp.username", "pcpadmin", "PCP username")
	pcpPassword   = flag.String("pcp.password", "", "PCP password")
)

func versionInfo() {
	fmt.Println(version.Print(exporterName))
	os.Exit(0)
}

func main() {
	flag.Parse()

	if *showVersion == true {
		versionInfo()
	}

	errChan := make(chan error, 10)
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, syscall.SIGINT, syscall.SIGTERM)

	logrus.Infof("Starting %s %s...", exporterName, version.Version)
	logrus.Infof("Listen address: %s", *listenAddress)

	options := pgpool2.Options{
		Username: *pcpUsername,
		Password: *pcpPassword,
		Hostname: *pcpHostname,
		Port:     *pcpPort,
		PassFile: *pcpPassFile,
	}

	pgpool2Client, err := pgpool2.NewClient(options)
	if err != nil {
		logrus.Fatal(err)
	}

	go func() {
		for {
			select {
			case err := <-errChan:
				if err != nil {
					pgpool2Client.Clean()
					logrus.Fatal(err)
				}
			case signal := <-signalChan:
				logrus.Infof("Captured %v. Exiting...", signal)
				pgpool2Client.Clean()
				logrus.Info("Bye")
				os.Exit(0)
			}
		}
	}()

	exporter := NewExporter(pgpool2Client)
	if err := prometheus.Register(exporter); err != nil {
		errChan <- err
	}

	http.Handle(*metricsPath, promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`<html>
			<head><title>` + exporterName + ` v` + version.Version + `</title></head>
			<body>
			<h1>` + exporterName + ` v` + version.Version + `</h1>
			<p><a href='` + *metricsPath + `'>Metrics</a></p>
			</body>
			</html>
		`))
	})

	errChan <- http.ListenAndServe(*listenAddress, nil)
}
