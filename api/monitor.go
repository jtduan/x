package api

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

type monitorSample struct {
	TS                int64  `json:"ts"`
	Conn8000Est       int    `json:"conn8000Est"`
	DownstreamRxTotal uint64 `json:"downstreamRxTotal"`
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
	conn := countTCPConnectionsEstablishedExcludeLocalPorts(8000, 8001, 18080)
	dsRxTotal := downstreamRxTotal.Load()

	s := monitorSample{
		TS:          now.Unix(),
		Conn8000Est: conn,
		DownstreamRxTotal: dsRxTotal,
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
	// Restore downstream RX counter from persisted samples (keep accumulating).
	series := m.series()
	if len(series) > 0 {
		last := series[len(series)-1]
		downstreamRxTotal.Store(last.DownstreamRxTotal)
	}
}

var downstreamRxTotal atomic.Uint64

type downstreamCountConn struct {
	net.Conn
}

func (c *downstreamCountConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 {
		downstreamRxTotal.Add(uint64(n))
	}
	return n, err
}

// WrapDownstreamConn wraps a downstream connection so that bytes read from it
// (downstream -> gost direction) are accumulated for monitor.
func WrapDownstreamConn(conn net.Conn) net.Conn {
	if conn == nil {
		return conn
	}
	return &downstreamCountConn{Conn: conn}
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
	io.WriteString(c.Writer, monitorHTMLLite)
}

type monitorAccessEntry struct {
	TS     int64  `json:"ts"`
	Method string `json:"method"`
	Host   string `json:"host"`
	URI    string `json:"uri"`
}

type monitorAccessStore struct {
	mu       sync.RWMutex
	capacity int
	next     int
	filled   bool
	entries  []monitorAccessEntry
}

func newMonitorAccessStore(capacity int) *monitorAccessStore {
	if capacity <= 0 {
		capacity = 200
	}
	return &monitorAccessStore{
		capacity: capacity,
		entries:  make([]monitorAccessEntry, capacity),
	}
}

func (s *monitorAccessStore) add(e monitorAccessEntry) {
	s.mu.Lock()
	s.entries[s.next] = e
	s.next = (s.next + 1) % s.capacity
	if s.next == 0 {
		s.filled = true
	}
	s.mu.Unlock()
}

func (s *monitorAccessStore) list(limit int) []monitorAccessEntry {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var out []monitorAccessEntry
	if !s.filled {
		out = append(out, s.entries[:s.next]...)
	} else {
		out = make([]monitorAccessEntry, 0, s.capacity)
		out = append(out, s.entries[s.next:]...)
		out = append(out, s.entries[:s.next]...)
	}

	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

var monitorAccess = newMonitorAccessStore(200)

func RecordProxyAccess(localAddr string, method string, host string, uri string) {
	_, port, err := net.SplitHostPort(localAddr)
	if err != nil || port != "8000" {
		return
	}

	cleanHost := strings.TrimSpace(host)
	if h, _, err := net.SplitHostPort(cleanHost); err == nil {
		cleanHost = h
	}
	cleanHost = strings.Trim(cleanHost, "[]")

	cleanURI := strings.TrimSpace(uri)
	if u, err := url.Parse(cleanURI); err == nil && u != nil && u.Host != "" {
		cleanURI = u.RequestURI()
		if cleanHost == "" {
			cleanHost = u.Hostname()
		}
	}

	monitorAccess.add(monitorAccessEntry{
		TS:     time.Now().Unix(),
		Method: method,
		Host:   cleanHost,
		URI:    cleanURI,
	})
}

func getMonitorHistory(c *gin.Context) {
	limit := 200
	if s := c.Query("limit"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 {
			limit = n
		}
	}
	loc := time.FixedZone("UTC+8", 8*60*60)
	entries := monitorAccess.list(limit)
	var b strings.Builder
	for i := range entries {
		e := entries[i]
		ts := time.Unix(e.TS, 0).In(loc).Format("2006-01-02 15:04:05")
		b.WriteString("【")
		b.WriteString(ts)
		b.WriteString("】")
		b.WriteString(e.Method)
		b.WriteString(" ")
		b.WriteString(e.Host)
		b.WriteString(e.URI)
		b.WriteString("\n")
	}
	c.Header("Content-Type", "text/plain; charset=utf-8")
	c.String(http.StatusOK, b.String())
}

func countTCPConnectionsEstablishedExcludeLocalPorts(excludeLocalPorts ...int) int {
	ex := map[string]struct{}{}
	for _, p := range excludeLocalPorts {
		h := strings.ToUpper(hex.EncodeToString([]byte{byte(p >> 8), byte(p)}))
		ex[h] = struct{}{}
	}
	return countProcNetExcludeLocalPorts("/proc/net/tcp", ex) + countProcNetExcludeLocalPorts("/proc/net/tcp6", ex)
}

func countProcNetExcludeLocalPorts(path string, excludeLocalPorts map[string]struct{}) int {
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
		if _, ok := excludeLocalPorts[p]; ok {
			continue
		}
		count++
	}
	return count
}

const monitorHTMLLite = `<!doctype html><html><head><meta charset='utf-8'><meta name='viewport' content='width=device-width,initial-scale=1'><title>gost monitor</title><style>html,body{height:100%;margin:0;font-family:system-ui,-apple-system,Segoe UI,Roboto,Helvetica,Arial}#app{height:100%;display:flex;flex-direction:column}header{padding:10px 14px;border-bottom:1px solid #eee;display:flex;gap:12px;align-items:center}main{flex:1;min-height:0;overflow:auto;padding:10px;display:grid;grid-template-columns:1fr;gap:10px}.card{border:1px solid #eee;border-radius:8px;padding:8px;min-height:240px}.card h3{margin:0 0 6px 0;font-size:14px;font-weight:600;color:#111}.chart{height:220px}.list{height:220px;overflow:auto;white-space:pre;font-size:13px;line-height:1.4}</style></head><body><div id='app'><header><strong>gost monitor</strong><span id='status'>loading...</span></header><main><div class='card'><h3>History</h3><div id='history' class='list'></div></div><div class='card'><h3>TCP EST (downstream)</h3><div id='connNow' style='font-size:13px;color:#666;margin-bottom:6px'></div><div id='chartConn' class='chart'></div></div><div class='card'><h3>Downstream -> gost traffic (total)</h3><div id='rxNow' style='font-size:13px;color:#666;margin-bottom:6px'></div><div id='chartRxTotal' class='chart'></div></div><div class='card'><h3>Downstream -> gost traffic (rate)</h3><div id='rxRateNow' style='font-size:13px;color:#666;margin-bottom:6px'></div><div id='chartRxRate' class='chart'></div></div></main></div><script src='https://cdn.jsdelivr.net/npm/echarts@5/dist/echarts.min.js'></script><script>(function(){const statusEl=document.getElementById('status');const historyEl=document.getElementById('history');const connNowEl=document.getElementById('connNow');const rxNowEl=document.getElementById('rxNow');const rxRateNowEl=document.getElementById('rxRateNow');const chartConnEl=document.getElementById('chartConn');const chartRxEl=document.getElementById('chartRxTotal');const chartRxRateEl=document.getElementById('chartRxRate');const chartConn=chartConnEl?echarts.init(chartConnEl):null;const chartRx=chartRxEl?echarts.init(chartRxEl):null;const chartRxRate=chartRxRateEl?echarts.init(chartRxRateEl):null;function fmtTS(sec){const d=new Date((sec||0)*1000);const p=(n)=>String(n).padStart(2,'0');return d.getFullYear()+'-'+p(d.getMonth()+1)+'-'+p(d.getDate())+' '+p(d.getHours())+':'+p(d.getMinutes())+':'+p(d.getSeconds());}function fmtBytes(v){v=Number(v||0);if(v<1024)return v.toFixed(0)+' B';if(v<1024*1024)return (v/1024).toFixed(2)+' KB';if(v<1024*1024*1024)return (v/1024/1024).toFixed(2)+' MB';return (v/1024/1024/1024).toFixed(2)+' GB';}function fmtKBps(v){v=Number(v||0);return v.toFixed(2)+' KB/s';}function setLine(chart,ts,vals,fmt){if(!chart)return;chart.setOption({grid:{left:60,right:20,top:10,bottom:30},xAxis:{type:'category',data:ts,axisLabel:{hideOverlap:true}},yAxis:{type:'value',axisLabel:{formatter:(v)=>fmt?fmt(v):v}},series:[{type:'line',data:vals,smooth:true,showSymbol:false}]});}async function load(){try{const limit=300;const res=await fetch('series?limit='+limit,{cache:'no-store'});const j=await res.json();const data=(j&&j.data)||[];statusEl.textContent='points: '+data.length+' (limit '+limit+')';const ts=[];const tsSec=[];const conn=[];const rxTotal=[];for(let i=0;i<data.length;i++){const x=data[i]||{};ts.push(fmtTS(x.ts||0));tsSec.push(x.ts||0);conn.push(x.conn8000Est||0);rxTotal.push(x.downstreamRxTotal||0);}const rxRate=[];for(let i=0;i<rxTotal.length;i++){if(i===0){rxRate.push(0);continue;}const dt=(tsSec[i]-tsSec[i-1]);if(dt<=0){rxRate.push(0);continue;}const dr=(rxTotal[i]-rxTotal[i-1]);if(dr<0){rxRate.push(0);continue;}rxRate.push((dr/dt)/1024);}if(conn.length>0&&connNowEl){connNowEl.textContent='current: '+conn[conn.length-1];}if(rxTotal.length>0&&rxNowEl){rxNowEl.textContent='current: '+fmtBytes(rxTotal[rxTotal.length-1]);}if(rxRate.length>0&&rxRateNowEl){rxRateNowEl.textContent='current: '+fmtKBps(rxRate[rxRate.length-1]);}setLine(chartConn,ts,conn);setLine(chartRx,ts,rxTotal,fmtBytes);setLine(chartRxRate,ts,rxRate,fmtKBps);const hres=await fetch('history?limit=200',{cache:'no-store'});const ht=await hres.text();if(historyEl){historyEl.textContent=ht||'(empty)';}}catch(e){statusEl.textContent='error';}}load();setInterval(load,30000);window.addEventListener('resize',()=>{if(chartConn)chartConn.resize();if(chartRx)chartRx.resize();if(chartRxRate)chartRxRate.resize();});})();</script></body></html>`
