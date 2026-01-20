package api

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	xmetrics "github.com/go-gost/x/metrics"
)

type monitorSample struct {
	TS           int64   `json:"ts"`
	CPUPercent   float64 `json:"cpuPercent"`
	RSSBytes     uint64  `json:"rssBytes"`
	HeapAlloc    uint64  `json:"heapAlloc"`
	RxBytesPerS  float64 `json:"rxBytesPerS"`
	TxBytesPerS  float64 `json:"txBytesPerS"`
	RxBytesTotal float64 `json:"rxBytesTotal"`
	TxBytesTotal float64 `json:"txBytesTotal"`
	TrafficTotal float64 `json:"trafficBytesTotal"`
	Conn8000Est  int     `json:"conn8000Est"`
}

type monitorStore struct {
	IntervalSeconds int   `json:"intervalSeconds"`
	Capacity        int   `json:"capacity"`
	Next            int   `json:"next"`
	Filled          bool  `json:"filled"`
	Samples         []monitorSample `json:"samples"`
}

type monitor struct {
	mu sync.RWMutex

	path     string
	interval time.Duration
	capacity int

	store monitorStore

	prevProcTotal uint64
	prevSysTotal  uint64

	prevInBytes  float64
	prevOutBytes float64
	prevTrafficTS time.Time

	stopOnce sync.Once
	stopCh   chan struct{}
}

var (
	monitorOnce sync.Once
	monitorInst *monitor
)

func getMonitor() *monitor {
	monitorOnce.Do(func() {
		m := &monitor{
			interval: 30 * time.Second,
			capacity: 24 * 60 * 60 / 30,
			stopCh:   make(chan struct{}),
		}
		m.path = defaultMonitorPath()
		m.store = monitorStore{
			IntervalSeconds: int(m.interval.Seconds()),
			Capacity:        m.capacity,
			Samples:         make([]monitorSample, m.capacity),
		}

		xmetrics.Enable(true)

		m.load()
		go m.loop()
		monitorInst = m
	})
	return monitorInst
}

func defaultMonitorPath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return "monitor.json"
	}
	return filepath.Join(home, ".gost", "monitor.json")
}

func (m *monitor) stop() {
	m.stopOnce.Do(func() {
		close(m.stopCh)
	})
}

func (m *monitor) loop() {
	t := time.NewTicker(m.interval)
	defer t.Stop()

	m.sampleAndStore(time.Now())

	for {
		select {
		case <-t.C:
			m.sampleAndStore(time.Now())
		case <-m.stopCh:
			return
		}
	}
}

func (m *monitor) sampleAndStore(now time.Time) {
	cpu, _ := sampleProcessCPUPercent(&m.prevProcTotal, &m.prevSysTotal)
	rss := sampleRSSBytes()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	rxBps, txBps := m.sampleTraffic(now)
	inTotal, outTotal := sampleGostTransferTotals()
	conn := countTCPConnectionsEstablished(8000)

	s := monitorSample{
		TS:          now.Unix(),
		CPUPercent:  cpu,
		RSSBytes:    rss,
		HeapAlloc:   ms.HeapAlloc,
		RxBytesPerS: rxBps,
		TxBytesPerS: txBps,
		RxBytesTotal: inTotal,
		TxBytesTotal: outTotal,
		TrafficTotal: inTotal + outTotal,
		Conn8000Est: conn,
	}

	m.mu.Lock()
	m.store.Samples[m.store.Next] = s
	m.store.Next = (m.store.Next + 1) % m.capacity
	if m.store.Next == 0 {
		m.store.Filled = true
	}
	m.mu.Unlock()

	m.save()
}

func (m *monitor) sampleTraffic(now time.Time) (rxBps, txBps float64) {
	inTotal, outTotal := sampleGostTransferTotals()

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.prevTrafficTS.IsZero() {
		m.prevInBytes = inTotal
		m.prevOutBytes = outTotal
		m.prevTrafficTS = now
		return 0, 0
	}

	dt := now.Sub(m.prevTrafficTS).Seconds()
	if dt <= 0 {
		return 0, 0
	}

	rxBps = (inTotal - m.prevInBytes) / dt
	txBps = (outTotal - m.prevOutBytes) / dt
	if rxBps < 0 {
		rxBps = 0
	}
	if txBps < 0 {
		txBps = 0
	}

	m.prevInBytes = inTotal
	m.prevOutBytes = outTotal
	m.prevTrafficTS = now

	return rxBps, txBps
}

func (m *monitor) series() []monitorSample {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var out []monitorSample
	if !m.store.Filled {
		out = append(out, m.store.Samples[:m.store.Next]...)
		return out
	}
	out = make([]monitorSample, 0, m.capacity)
	out = append(out, m.store.Samples[m.store.Next:]...)
	out = append(out, m.store.Samples[:m.store.Next]...)
	return out
}

func (m *monitor) load() {
	path := m.path
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var st monitorStore
	if err := json.Unmarshal(b, &st); err != nil {
		return
	}
	if st.Capacity != m.capacity || st.IntervalSeconds != int(m.interval.Seconds()) {
		return
	}
	if len(st.Samples) != m.capacity {
		return
	}
	m.mu.Lock()
	m.store = st
	m.mu.Unlock()
}

func (m *monitor) save() {
	path := m.path
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}

	m.mu.RLock()
	b, err := json.Marshal(m.store)
	m.mu.RUnlock()
	if err != nil {
		return
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

func getMonitorSeries(c *gin.Context) {
	m := getMonitor()
	series := m.series()
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			if len(series) > n {
				series = series[len(series)-n:]
			}
		}
	}
	c.JSON(http.StatusOK, Response{Data: series})
}

func getMonitorUI(c *gin.Context) {
	c.Header("Content-Type", "text/html; charset=utf-8")
	io.WriteString(c.Writer, monitorHTML)
}

const monitorHTML = `<!doctype html><html><head><meta charset='utf-8'><meta name='viewport' content='width=device-width,initial-scale=1'><title>gost monitor</title><style>html,body{height:100%;margin:0;font-family:system-ui,-apple-system,Segoe UI,Roboto,Helvetica,Arial}#app{height:100%;display:flex;flex-direction:column}header{padding:10px 14px;border-bottom:1px solid #eee;display:flex;gap:12px;align-items:center}main{flex:1;min-height:0;overflow:auto;padding:10px;display:grid;grid-template-columns:1fr;gap:10px}.card{border:1px solid #eee;border-radius:8px;padding:8px;min-height:240px}.card h3{margin:0 0 6px 0;font-size:14px;font-weight:600;color:#111}.chart{height:220px}</style></head><body><div id='app'><header><strong>gost monitor (multi-charts v2)</strong><span id='status'>loading...</span></header><main><div class='card'><h3>CPU (%)</h3><div id='chartCpu' class='chart'></div></div><div class='card'><h3>RSS (bytes)</h3><div id='chartRss' class='chart'></div></div><div class='card'><h3>HeapAlloc (bytes)</h3><div id='chartHeap' class='chart'></div></div><div class='card'><h3>RX (bytes/s)</h3><div id='chartRx' class='chart'></div></div><div class='card'><h3>TX (bytes/s)</h3><div id='chartTx' class='chart'></div></div><div class='card'><h3>Traffic total (RX+TX cumulative)</h3><div id='chartTrafficTotal' class='chart'></div></div><div class='card'><h3>TCP EST :8000 (count)</h3><div id='chartConn8000' class='chart'></div></div></main></div><script src='https://cdn.jsdelivr.net/npm/echarts@5/dist/echarts.min.js'></script><script>(function(){const statusEl=document.getElementById('status');function fmtBytes(v){if(v<1024)return v.toFixed(0)+' B';if(v<1024*1024)return (v/1024).toFixed(1)+' KB';if(v<1024*1024*1024)return (v/1024/1024).toFixed(1)+' MB';return (v/1024/1024/1024).toFixed(2)+' GB';}function fmtBps(v){return fmtBytes(v)+'/s';}function makeChart(id){const el=document.getElementById(id);if(!el)return null;return echarts.init(el);}const charts={cpu:makeChart('chartCpu'),rss:makeChart('chartRss'),heap:makeChart('chartHeap'),rx:makeChart('chartRx'),tx:makeChart('chartTx'),trafficTotal:makeChart('chartTrafficTotal'),conn8000:makeChart('chartConn8000')};function setLine(chart,title,ts,vals,yFmt){if(!chart)return;chart.setOption({tooltip:{trigger:'axis',valueFormatter:yFmt||((v)=>v)},grid:{left:50,right:20,top:10,bottom:30},xAxis:{type:'time'},yAxis:{type:'value',axisLabel:{formatter:yFmt?((v)=>yFmt(v)):undefined}},series:[{name:title,type:'line',showSymbol:false,data:ts.map((t,i)=>[t,vals[i]])}]});}
async function load(){try{const limit=300;const res=await fetch('series?limit='+limit,{cache:'no-store'});const j=await res.json();const data=(j&&j.data)||[];statusEl.textContent='points: '+data.length+' (limit '+limit+')';const ts=[];const cpu=[];const rss=[];const heap=[];const rx=[];const tx=[];const trafficTotal=[];const c8000=[];for(let i=0;i<data.length;i++){const x=data[i]||{};ts.push((x.ts||0)*1000);cpu.push(x.cpuPercent||0);rss.push(x.rssBytes||0);heap.push(x.heapAlloc||0);rx.push(x.rxBytesPerS||0);tx.push(x.txBytesPerS||0);trafficTotal.push(x.trafficBytesTotal||0);c8000.push(x.conn8000Est||0);}setLine(charts.cpu,'CPU%',ts,cpu);setLine(charts.rss,'RSS',ts,rss,fmtBytes);setLine(charts.heap,'HeapAlloc',ts,heap,fmtBytes);setLine(charts.rx,'RX',ts,rx,fmtBps);setLine(charts.tx,'TX',ts,tx,fmtBps);setLine(charts.trafficTotal,'Traffic total',ts,trafficTotal,fmtBytes);setLine(charts.conn8000,':8000 EST',ts,c8000);}catch(e){statusEl.textContent='error';}}load();setInterval(load,30000);window.addEventListener('resize',()=>{Object.keys(charts).forEach(k=>{if(charts[k])charts[k].resize();});});})();</script></body></html>`

func sampleGostTransferTotals() (inTotal, outTotal float64) {
	mfs, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		return 0, 0
	}
	for _, mf := range mfs {
		if mf == nil || mf.Name == nil {
			continue
		}
		switch *mf.Name {
		case "gost_service_transfer_input_bytes_total":
			inTotal += sumMetricFamily(mf)
		case "gost_service_transfer_output_bytes_total":
			outTotal += sumMetricFamily(mf)
		}
	}
	return
}

func sumMetricFamily(mf *dto.MetricFamily) float64 {
	var sum float64
	for _, m := range mf.GetMetric() {
		if m == nil {
			continue
		}
		if c := m.GetCounter(); c != nil {
			sum += c.GetValue()
		}
		if g := m.GetGauge(); g != nil {
			sum += g.GetValue()
		}
	}
	return sum
}

func sampleRSSBytes() uint64 {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(b))
	if len(fields) < 2 {
		return 0
	}
	rssPages, err := strconv.ParseUint(fields[1], 10, 64)
	if err != nil {
		return 0
	}
	pageSize := uint64(os.Getpagesize())
	return rssPages * pageSize
}

func sampleProcessCPUPercent(prevProcTotal, prevSysTotal *uint64) (float64, error) {
	procTotal, err := readProcTotalTicks()
	if err != nil {
		return 0, err
	}
	sysTotal, err := readSystemTotalTicks()
	if err != nil {
		return 0, err
	}

	if *prevProcTotal == 0 || *prevSysTotal == 0 {
		*prevProcTotal = procTotal
		*prevSysTotal = sysTotal
		return 0, nil
	}

	dProc := float64(procTotal - *prevProcTotal)
	dSys := float64(sysTotal - *prevSysTotal)
	*prevProcTotal = procTotal
	*prevSysTotal = sysTotal
	if dSys <= 0 {
		return 0, nil
	}
	cpu := (dProc / dSys) * float64(runtime.NumCPU()) * 100
	if cpu < 0 {
		cpu = 0
	}
	return cpu, nil
}

func readProcTotalTicks() (uint64, error) {
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0, err
	}
	s := string(b)
	rp := strings.LastIndexByte(s, ')')
	if rp < 0 {
		return 0, errors.New("invalid /proc/self/stat")
	}
	fields := strings.Fields(s[rp+1:])
	if len(fields) < 15 {
		return 0, errors.New("invalid /proc/self/stat fields")
	}
	utime, err := strconv.ParseUint(fields[11], 10, 64)
	if err != nil {
		return 0, err
	}
	stime, err := strconv.ParseUint(fields[12], 10, 64)
	if err != nil {
		return 0, err
	}
	return utime + stime, nil
}

func readSystemTotalTicks() (uint64, error) {
	f, err := os.Open("/proc/stat")
	if err != nil {
		return 0, err
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	for s.Scan() {
		line := s.Text()
		if strings.HasPrefix(line, "cpu ") {
			fields := strings.Fields(line)
			var sum uint64
			for i := 1; i < len(fields); i++ {
				v, err := strconv.ParseUint(fields[i], 10, 64)
				if err != nil {
					return 0, err
				}
				sum += v
			}
			return sum, nil
		}
	}
	if err := s.Err(); err != nil {
		return 0, err
	}
	return 0, errors.New("cpu line not found in /proc/stat")
}

func countTCPConnectionsEstablished(port int) int {
	hexPort := strings.ToUpper(hex.EncodeToString([]byte{byte(port >> 8), byte(port)}))
	return countProcNet("/proc/net/tcp", hexPort) + countProcNet("/proc/net/tcp6", hexPort)
}

func countProcNet(path, hexPort string) int {
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer f.Close()

	s := bufio.NewScanner(f)
	first := true
	count := 0
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if first {
			first = false
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		local := fields[1]
		st := fields[3]
		if st != "01" {
			continue
		}
		idx := strings.LastIndex(local, ":")
		if idx < 0 {
			continue
		}
		p := strings.ToUpper(local[idx+1:])
		if p == hexPort {
			count++
		}
	}
	return count
}
