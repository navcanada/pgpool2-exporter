package main

import (
	"strconv"
	"time"

	"fmt"

	"github.com/navcanada/pgpool2-exporter/pgpool2"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/version"
	"github.com/sirupsen/logrus"
)

const (
	namespace    = "pgpool2"
	exporterName = "pgpool2_exporter"
)

var (
	PoolLastScrapeError = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "last_scrape_error"),
		"Whether the last scrape of metrics from Pgpool2 resulted in an error (1 for error, 0 for success)",
		nil, nil,
	)
	PoolLastScrapeDuration = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "last_scrape_duration_seconds"),
		"Duration of the last scrape of metrics from Pgpool2",
		nil, nil,
	)
	PoolNodeCount = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "node_count"),
		"Displays the total number of database nodes",
		nil, nil,
	)
	PoolNodeInfo = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "node_info"),
		"Displays the information of node",
		[]string{"id", "node", "port", "width", "role", "replicationDelay", "replicationState", "replicationSyncState", "lastStatusChange"}, nil,
	)
	PoolProcCount = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "proc_count"),
		"Displays number of all Pgpool-II children processes",
		nil, nil,
	)
	PoolNumberActiveConnections = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "frontend_active_connections"),
		"Displays number of all active connections to all Pgpool-II children processes",
		[]string{"database"}, nil,
	)
	PoolNumberInactiveConnections = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "", "frontend_inactive_connections"),
		"Displays number of all inactive connections to all Pgpool-II children processes",
		[]string{"database"}, nil,
	)
	WatchdogTotalNodes = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "watchdog", "nodes_total"),
		"Watchdog total nodes",
		nil, nil,
	)
	WatchdogRemoteNodes = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "watchdog", "nodes_remote"),
		"Watchdog remote nodes",
		nil, nil,
	)
	WatchdogAliveRemoteNodes = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "watchdog", "nodes_alive_remote"),
		"Watchdog alive remote nodes",
		nil, nil,
	)
	WatchdogVIP = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "watchdog", "vip"),
		"Watchdog virtual IP",
		nil, nil,
	)
	WatchdogQuorumState = prometheus.NewDesc(
		prometheus.BuildFQName(namespace, "watchdog", "quorum_state"),
		"Watchdog quorum state (1 is ok)",
		nil, nil,
	)
)

type Exporter struct {
	pgpool *pgpool2.Client
}

func init() {
	prometheus.MustRegister(version.NewCollector(exporterName))
}

func NewExporter(pgpool *pgpool2.Client) *Exporter {
	return &Exporter{
		pgpool: pgpool,
	}
}

func (e *Exporter) collectNodeMetrics(ch chan<- prometheus.Metric) error {
	nodeCount, err := e.pgpool.ExecNodeCount()
	if err != nil {
		return fmt.Errorf("ExecNodeCount() error: %v", err)
	}
	ch <- prometheus.MustNewConstMetric(
		PoolNodeCount,
		prometheus.GaugeValue,
		float64(nodeCount),
	)
	for i := 0; i < nodeCount; i++ {
		nodeInfo, err := e.pgpool.ExecNodeInfo(i)
		if err != nil {
			return fmt.Errorf("ExecNodeInfo(%d) error: %v", i, err)
		}
		ch <- prometheus.MustNewConstMetric(
			PoolNodeInfo,
			prometheus.GaugeValue,
			float64(nodeInfo.StatusCode),
			strconv.Itoa(i),
			nodeInfo.Hostname,
			strconv.Itoa(nodeInfo.Port),
			strconv.FormatFloat(nodeInfo.Weight, 'f', 6, 64),
			nodeInfo.Role,
			strconv.FormatFloat(nodeInfo.ReplicationDelay, 'f', 6, 64),
			nodeInfo.ReplicationState,
			nodeInfo.ReplicationSyncState,
			nodeInfo.LastStatusChange,
		)
	}
	return nil
}

func (e *Exporter) collectProcCountMetrics(ch chan<- prometheus.Metric) error {
	procArr, err := e.pgpool.ExecProcCount()
	if err != nil {
		return fmt.Errorf("ExecProcCount() error: %v", err)
	}
	ch <- prometheus.MustNewConstMetric(
		PoolProcCount,
		prometheus.GaugeValue,
		float64(len(procArr)),
	)
	return nil
}

func (e *Exporter) collectProcInfoMetrics(ch chan<- prometheus.Metric) error {
	procInfoArr, err := e.pgpool.ExecProcInfo()
	if err != nil {
		return fmt.Errorf("ExecProcInfo() error: %v", err)
	}
	procSummary := e.pgpool.ProcInfoSummary(procInfoArr)
	for database, counter := range procSummary.Active {
		ch <- prometheus.MustNewConstMetric(
			PoolNumberActiveConnections,
			prometheus.GaugeValue,
			float64(counter),
			database,
		)
	}
	for database, counter := range procSummary.Inactive {
		ch <- prometheus.MustNewConstMetric(
			PoolNumberInactiveConnections,
			prometheus.GaugeValue,
			float64(counter),
			database,
		)
	}
	return nil
}

func (e *Exporter) collectWatchdogInfoMetrics(ch chan<- prometheus.Metric) error {
	watchdogInfo, err := e.pgpool.ExecWatchdogInfo()
	if err != nil {
		return fmt.Errorf("ExecWatchdogInfo() error: %v", err)
	}
	ch <- prometheus.MustNewConstMetric(
		WatchdogTotalNodes,
		prometheus.GaugeValue,
		float64(watchdogInfo.TotalNodes),
	)
	ch <- prometheus.MustNewConstMetric(
		WatchdogRemoteNodes,
		prometheus.GaugeValue,
		float64(watchdogInfo.RemoteNodes),
	)
	ch <- prometheus.MustNewConstMetric(
		WatchdogAliveRemoteNodes,
		prometheus.GaugeValue,
		float64(watchdogInfo.AliveRemoteNodes),
	)
	ch <- prometheus.MustNewConstMetric(
		WatchdogQuorumState,
		prometheus.GaugeValue,
		float64(watchdogInfo.QuorumStateCode),
	)
	if watchdogInfo.VIP {
		ch <- prometheus.MustNewConstMetric(
			WatchdogVIP,
			prometheus.GaugeValue,
			1.0,
		)
	} else {
		ch <- prometheus.MustNewConstMetric(
			WatchdogVIP,
			prometheus.GaugeValue,
			0.0,
		)
	}
	return nil
}

func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	var scrapeError bool

	defer func(begun time.Time) {
		ch <- prometheus.MustNewConstMetric(
			PoolLastScrapeDuration,
			prometheus.GaugeValue,
			time.Since(begun).Seconds(),
		)
	}(time.Now())

	if err := e.collectNodeMetrics(ch); err != nil {
		scrapeError = true
		logrus.Error(err)
	}

	if err := e.collectProcCountMetrics(ch); err != nil {
		scrapeError = true
		logrus.Error(err)
	}

	if err := e.collectProcInfoMetrics(ch); err != nil {
		scrapeError = true
		logrus.Error(err)
	}

	if err := e.collectWatchdogInfoMetrics(ch); err != nil {
		scrapeError = true
		logrus.Error(err)
	}

	scrapeErrorFloat := 0.0
	if scrapeError {
		scrapeErrorFloat = 1.0
	}

	ch <- prometheus.MustNewConstMetric(
		PoolLastScrapeError,
		prometheus.GaugeValue,
		scrapeErrorFloat,
	)
}

func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	ch <- PoolLastScrapeError
	ch <- PoolLastScrapeDuration
	ch <- PoolNodeCount
	ch <- PoolProcCount
	ch <- PoolNodeInfo
	ch <- PoolNumberActiveConnections
	ch <- PoolNumberInactiveConnections
	ch <- WatchdogTotalNodes
	ch <- WatchdogRemoteNodes
	ch <- WatchdogAliveRemoteNodes
	ch <- WatchdogQuorumState
	ch <- WatchdogVIP
}
