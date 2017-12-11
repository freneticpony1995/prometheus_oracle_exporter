package main

import (
	"database/sql"
	"flag"
	"net/http"
	"os"
	"strings"
	"time"

	_ "github.com/mattn/go-oci8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
)


var (
	// Version will be set at build time.
	Version       = "0.0.4"
	listenAddress = flag.String("web.listen-address", ":9161", "Address to listen on for web interface and telemetry.")
	metricPath    = flag.String("web.telemetry-path", "/metrics", "Path under which to expose metrics.")
	landingPage   = []byte("<html><head><title>Prometheus Oracle exporter</title></head><body><h1>Prometheus Oracle exporter</h1><p><a href='" + *metricPath + "'>Metrics</a></p></body></html>")
)

// Metric name parts.
const (
	namespace = "oracledb"
	exporter  = "exporter"
)


type dbConn struct {
	database, instance string
        db                 *sql.DB
}

// Exporter collects Oracle DB metrics. It implements prometheus.Collector.
type Exporter struct {
	dsn             string
	duration, error prometheus.Gauge
	totalScrapes    prometheus.Counter
	scrapeErrors    *prometheus.CounterVec
        session         *prometheus.GaugeVec
        sysstat         *prometheus.GaugeVec
        waitclass       *prometheus.GaugeVec
	sysmetric   	*prometheus.GaugeVec
	interconnect   	*prometheus.GaugeVec
	uptime          *prometheus.GaugeVec
	tablespace      *prometheus.GaugeVec
	recovery        *prometheus.GaugeVec
	redo            *prometheus.GaugeVec
	cache           *prometheus.GaugeVec
        conns           []dbConn
}


// NewExporter returns a new Oracle DB exporter for the provided DSN.
func NewExporter(dsn string) *Exporter {
	return &Exporter{
		dsn: dsn,
		duration: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: exporter,
			Name:      "last_scrape_duration_seconds",
			Help:      "Duration of the last scrape of metrics from Oracle DB.",
		}),
		totalScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: exporter,
			Name:      "scrapes_total",
			Help:      "Total number of times Oracle DB was scraped for metrics.",
		}),
		scrapeErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: exporter,
			Name:      "scrape_errors_total",
			Help:      "Total number of times an error occured scraping a Oracle database.",
		}, []string{"collector"}),
		error: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: exporter,
			Name:      "last_scrape_error",
			Help:      "Whether the last scrape of metrics from Oracle DB resulted in an error (1 for error, 0 for success).",
		}),
		sysmetric: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "sysmetric",
			Help:      "Gauge metric with read/write pysical IOPs/bytes (v$sysmetric).",
		}, []string{"type","database","dbinstance"}),
		waitclass: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "waitclass",
			Help:      "Gauge metric with Waitevents (v$waitclassmetric).",
		}, []string{"type","database","dbinstance"}),
		sysstat: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "sysstat",
			Help:      "Gauge metric with commits/rollbacks/parses (v$sysstat).",
		}, []string{"type","database","dbinstance"}),
		session: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "session",
			Help:      "Gauge metric user/system active/passive sessions (v$session).",
		}, []string{"type","state","database","dbinstance"}),
		uptime: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "uptime",
			Help:      "Gauge metric with uptime in days of the Instance.",
		}, []string{"database","dbinstance"}),
		tablespace: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "tablespace",
			Help:      "Gauge metric with total/free size of the Tablespaces.",
		}, []string{"name","type","contents","database","dbinstance"}),
		interconnect: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "interconnect",
			Help:      "Gauge metric with interconnect block transfers (v$sysstat).",
		}, []string{"type","database","dbinstance"}),
		recovery: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "recovery",
			Help:      "Gauge metric with percentage usage of FRA (v$recovery_file_dest).",
		}, []string{"database","dbinstance"}),
		redo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "redo",
			Help:      "Gauge metric with Redo log switches over last 5 min (v$log_history).",
		}, []string{"database","dbinstance"}),
		cache: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "cachehitratio",
			Help:      "Gauge metric witch Cache hit ratios (v$sysmetric).",
		}, []string{"type","database","dbinstance"}),
	}
}

// ScrapeCache collects session metrics from the v$sysmetrics view.
func (e *Exporter) ScrapeCache() {
	var (
		rows *sql.Rows
		err  error
	)
	for _, conn := range e.conns {        
		//metric_id	metric_name
		//2000		Buffer Cache Hit Ratio
		//2050		Cursor Cache Hit Ratio
		//2112		Library Cache Hit Ratio
		//2110		Row Cache Hit Ratio

	  	rows, err = conn.db.Query("select metric_name,value from v$sysmetric where metric_id in (2000,2050,2112,2110)")
		if err != nil {
			break
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var value float64
			if err := rows.Scan(&name, &value); err != nil {
				break
			}
			name = cleanName(name)
	                e.cache.WithLabelValues(name,conn.database,conn.instance).Set(value)
		}
	}
}


// ScrapeRecovery collects tablespace metrics
func (e *Exporter) ScrapeRedo() {
	var (
		rows *sql.Rows
		err  error
	)
	for _, conn := range e.conns {        
	  	rows, err = conn.db.Query(`select count(*) from v$log_history where first_time > sysdate - 1/24/12`)
		if err != nil {
			break
		}
		defer rows.Close()
		for rows.Next() {
			var value float64
			if err := rows.Scan(&value); err != nil {
				break
			}
	                e.redo.WithLabelValues(conn.database,conn.instance).Set(value)
		}
	}
}

// ScrapeRecovery collects tablespace metrics
func (e *Exporter) ScrapeRecovery() {
	var (
		rows *sql.Rows
		err  error
	)
	for _, conn := range e.conns {        
	  	rows, err = conn.db.Query(`SELECT sum(percent_space_used)-sum(percent_space_reclaimable) percent_used 
                                           from V$FLASH_RECOVERY_AREA_USAGE`)
		if err != nil {
			break
		}
		defer rows.Close()
		for rows.Next() {
			var value float64
			if err := rows.Scan(&value); err != nil {
				break
			}
	                e.recovery.WithLabelValues(conn.database,conn.instance).Set(value)
		}
	}
}

// ScrapeTablespaces collects tablespace metrics
func (e *Exporter) ScrapeInterconnect() {
	var (
		rows *sql.Rows
		err  error
	)
	for _, conn := range e.conns {        
	  	rows, err = conn.db.Query(`SELECT name, value
                                           FROM V$SYSSTAT
                                           WHERE name in ('gc cr blocks served','gc cr blocks flushed','gc cr blocks received')`)
		if err != nil {
			break
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var value float64
			if err := rows.Scan(&name, &value); err != nil {
				break
			}
			name = cleanName(name)
	                e.interconnect.WithLabelValues(name,conn.database,conn.instance).Set(value)
		}
	}
}

// ScrapeTablespaces collects tablespace metrics
func (e *Exporter) ScrapeTablespace() {
	var (
		rows *sql.Rows
		err  error
	)
	for _, conn := range e.conns {        
	  	rows, err = conn.db.Query(`WITH 
                                             getsize AS (SELECT tablespace_name, SUM(bytes) tsize 
                                                         FROM dba_data_files GROUP BY tablespace_name),
                                             getfree as (SELECT tablespace_name, contents, SUM(blocks*block_size) tfree 
                                                         FROM DBA_LMT_FREE_SPACE a, v$tablespace b, dba_tablespaces c 
                                                         WHERE a.TABLESPACE_ID= b.ts# and b.name=c.tablespace_name 
                                                         GROUP BY tablespace_name,contents)
                                           SELECT a.tablespace_name, b.contents, a.tsize,  b.tfree
                                           FROM GETSIZE a, GETFREE b 
                                           WHERE a.tablespace_name = b.tablespace_name
					   UNION
                                           SELECT tablespace_name, 'TEMPORARY', sum(tablespace_size), sum(free_space) 
                                           FROM dba_temp_free_space 
                                           GROUP BY tablespace_name`)
		if err != nil {
			break
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var contents string
			var tsize float64
			var tfree float64
			if err := rows.Scan(&name, &contents, &tsize, &tfree); err != nil {
				break
			}
	                e.tablespace.WithLabelValues(name,"total",contents,conn.database,conn.instance).Set(tsize)
	                e.tablespace.WithLabelValues(name,"free",contents,conn.database,conn.instance).Set(tfree)
	                e.tablespace.WithLabelValues(name,"used",contents,conn.database,conn.instance).Set(tsize-tfree)
		}
	}
}

// ScrapeSessions collects session metrics from the v$session view.
func (e *Exporter) ScrapeSession() {
	var (
		rows *sql.Rows
		err  error
	)
	for _, conn := range e.conns {        
	  	rows, err = conn.db.Query(`SELECT decode(username,NULL,'SYSTEM','SYS','SYSTEM','USER'), status,count(*) 
                                           FROM v$session 
                                           GROUP BY decode(username,NULL,'SYSTEM','SYS','SYSTEM','USER'),status`)
		if err != nil {
			break
		}
		defer rows.Close()
		for rows.Next() {
			var user string
			var status string
			var value float64
			if err := rows.Scan(&user, &status, &value); err != nil {
				break
			}
	                e.session.WithLabelValues(user,status,conn.database,conn.instance).Set(value)
		}
	}
}


// ScrapeUptime Instance uptime
func (e *Exporter) ScrapeUptime() {
	var uptime float64
	for _, conn := range e.conns {        
                err := conn.db.QueryRow("select sysdate-startup_time from v$instance").Scan(&uptime)
		if err != nil {
			return
		}
	        e.uptime.WithLabelValues(conn.database,conn.instance).Set(uptime)
	}
}

// ScrapeSysstat collects activity metrics from the v$sysstat view.
func (e *Exporter) ScrapeSysstat() {
	var (
		rows *sql.Rows
		err  error
	)
	for _, conn := range e.conns {        
	  	rows, err = conn.db.Query(`SELECT name, value FROM v$sysstat 
                                           WHERE name IN ('parse count (total)', 'execute count', 'user commits', 'user rollbacks')`)
		if err != nil {
			break
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var value float64
			if err := rows.Scan(&name, &value); err != nil {
				break
			}
			name = cleanName(name)
	                e.sysstat.WithLabelValues(name,conn.database,conn.instance).Set(value)
		}
	}
}

// ScrapeWaitTime collects wait time metrics from the v$waitclassmetric view.
func (e *Exporter) ScrapeWaitclass() {
	var (
		rows *sql.Rows
		err  error
	)
	for _, conn := range e.conns {        
	  	rows, err = conn.db.Query(`SELECT n.wait_class, round(m.time_waited/m.INTSIZE_CSEC,3)
                                           FROM v$waitclassmetric  m, v$system_wait_class n 
                                           WHERE m.wait_class_id=n.wait_class_id and n.wait_class != 'Idle'`)
		if err != nil {
			break
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var value float64
			if err := rows.Scan(&name, &value); err != nil {
				break
			}
			name = cleanName(name)
	                e.waitclass.WithLabelValues(name,conn.database,conn.instance).Set(value)
		}
	}
}

// ScrapeSysmetrics collects session metrics from the v$sysmetrics view.
func (e *Exporter) ScrapeSysmetric() {
	var (
		rows *sql.Rows
		err  error
	)
	for _, conn := range e.conns {        
		//metric_id	metric_name
		//2092		Physical Read Total IO Requests Per Sec
		//2093		Physical Read Total Bytes Per Sec
		//2100		Physical Write Total IO Requests Per Sec
		//2124		Physical Write Total Bytes Per Sec
	  	rows, err = conn.db.Query("select metric_name,value from v$sysmetric where metric_id in (2092,2093,2124,2100)")
		if err != nil {
			break
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			var value float64
			if err := rows.Scan(&name, &value); err != nil {
				break
			}
			name = cleanName(name)
	                e.sysmetric.WithLabelValues(name,conn.database,conn.instance).Set(value)
		}
	}
}


// Describe describes all the metrics exported by the Oracle exporter.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	e.duration.Describe(ch)
	e.totalScrapes.Describe(ch)
	e.scrapeErrors.Describe(ch)
        e.session.Describe(ch)
        e.sysstat.Describe(ch)
        e.waitclass.Describe(ch)
	e.sysmetric.Describe(ch)
	e.interconnect.Describe(ch)
        e.tablespace.Describe(ch)
        e.recovery.Describe(ch)
        e.redo.Describe(ch)
        e.cache.Describe(ch)
	e.uptime.Describe(ch)
}

// Connect the DBs and gather Databasename and Instancename
func (e *Exporter) Connect() {
        var instance string
        var database string
	for _, dsn := range strings.Split(e.dsn,";") {
		db , err := sql.Open("oci8", dsn)
 	  	if err != nil {
			log.Errorln("Error opening connection to database:", err)
			break
		}
		err = db.QueryRow("select db_unique_name from v$database").Scan(&database)
		if err != nil {
			log.Errorln("Error query the database name:", err)
			break
		}

		err = db.QueryRow("select instance_name from v$instance").Scan(&instance)
		if err != nil {
			log.Errorln("Error query the instance name:", err)
			break
		}
                conn := dbConn{database: database, instance: instance, db: db}
                e.conns = append(e.conns, conn)
	}
}

// Close Connections
func (e *Exporter) Close() {
	for _, conn := range e.conns {
           conn.db.Close()
	}
        e.conns = nil
}


// Collect implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {      
	var err error
        e.totalScrapes.Inc()
	defer func(begun time.Time) {
		e.duration.Set(time.Since(begun).Seconds())
		if err == nil {
			e.error.Set(0)
		} else {
			e.error.Set(1)
		}
	}(time.Now())

        e.Connect()

        e.ScrapeUptime()
        e.uptime.Collect(ch)

        e.ScrapeSession()
        e.session.Collect(ch)

        e.ScrapeSysstat()
        e.sysstat.Collect(ch)

        e.ScrapeWaitclass()
        e.waitclass.Collect(ch)

        e.ScrapeSysmetric()
        e.sysmetric.Collect(ch)

        e.ScrapeTablespace()
        e.tablespace.Collect(ch)

        e.ScrapeInterconnect()
        e.interconnect.Collect(ch)

        e.ScrapeRecovery()
        e.recovery.Collect(ch)

        e.ScrapeRedo()
        e.redo.Collect(ch)

        e.ScrapeCache()
        e.cache.Collect(ch)

	ch <- e.duration
	ch <- e.totalScrapes
	ch <- e.error
	e.scrapeErrors.Collect(ch)

        e.Close()
}

// Oracle gives us some ugly names back. This function cleans things up for Prometheus.
func cleanName(s string) string {
	s = strings.Replace(s, " ", "_", -1) // Remove spaces
	s = strings.Replace(s, "(", "", -1)  // Remove open parenthesis
	s = strings.Replace(s, ")", "", -1)  // Remove close parenthesis
	s = strings.Replace(s, "/", "", -1)  // Remove forward slashes
	s = strings.ToLower(s)
	return s
}

func main() {
	flag.Parse()
	log.Infoln("Starting Prometheus Oracle exporter " + Version)
	dsn := os.Getenv("DATA_SOURCE_NAME")
	exporter := NewExporter(dsn)
	prometheus.MustRegister(exporter)
	http.Handle(*metricPath, prometheus.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Write(landingPage)
	})
	log.Infoln("Listening on", *listenAddress)
	log.Fatal(http.ListenAndServe(*listenAddress, nil))
}
