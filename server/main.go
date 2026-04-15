package main

import (
	"bufio"
	"crypto/subtle"
	"embed"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ── Config (override via env) ─────────────────────────────────────────────────

var (
	listenAddr  = env("CAMS_ADDR", ":7000")
	tcpAddr     = env("CAMS_TCP_ADDR", ":7001")
	recDir      = env("CAMS_REC_DIR", "recordings")
	authUser    = env("CAMS_USER", "admin")
	authPass    = env("CAMS_PASS", "changeme")
	maxDiskGB   = int64(1)          // GB limit
	segmentDur  = 10 * time.Minute // MP4 segment length
	recFPS      = 15               // expected camera fps for ffmpeg
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ── MP4 segment recorder ───────────────────────────────────────────────────────

type recorder struct {
	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	started time.Time
	path    string
}

func startRecorder(outPath string) (*recorder, error) {
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return nil, err
	}
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "image2pipe",
		"-framerate", fmt.Sprintf("%d", recFPS),
		"-vcodec", "mjpeg",
		"-i", "pipe:0",
		"-c:v", "libx264",
		"-crf", "26",
		"-preset", "veryfast",
		"-movflags", "+faststart",
		outPath,
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	log.Printf("[rec] started segment: %s", outPath)
	return &recorder{cmd: cmd, stdin: stdin, started: time.Now(), path: outPath}, nil
}

func (r *recorder) write(frame []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stdin != nil {
		r.stdin.Write(frame)
	}
}

func (r *recorder) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stdin != nil {
		r.stdin.Close()
		r.stdin = nil
	}
	if r.cmd != nil {
		r.cmd.Wait()
		r.cmd = nil
		log.Printf("[rec] closed segment: %s", r.path)
	}
}

// ── Per-camera state ───────────────────────────────────────────────────────────

type CamSettings struct {
	Brightness  int  `json:"brightness"`
	Contrast    int  `json:"contrast"`
	Saturation  int  `json:"saturation"`
	AELevel     int  `json:"ae_level"`
	NightMode   bool `json:"night_mode"`
	JpegQuality int  `json:"jpeg_quality"`
	FrameSize   int  `json:"frame_size"`
}

type cam struct {
	mu       sync.RWMutex
	latest   []byte
	subs     []chan []byte
	online   bool
	lastSeen time.Time
	settings CamSettings
	rec      *recorder
}

func newCam() *cam {
	return &cam{settings: CamSettings{
		Brightness: 0, Contrast: 0, Saturation: -2,
		JpegQuality: 12, FrameSize: 5,
	}}
}

func (c *cam) publish(id string, frame []byte) {
	cp := make([]byte, len(frame))
	copy(cp, frame)

	c.mu.Lock()
	c.latest = cp
	c.online = true
	c.lastSeen = time.Now()

	// rotate recorder if needed
	if c.rec == nil || time.Since(c.rec.started) >= segmentDur {
		old := c.rec
		dir := filepath.Join(recDir, id, time.Now().Format("2006-01-02"))
		path := filepath.Join(dir, time.Now().Format("15-04-05")+".mp4")
		if r, err := startRecorder(path); err == nil {
			c.rec = r
		} else {
			log.Printf("[%s] recorder start error: %v", id, err)
		}
		if old != nil {
			go old.close()
		}
	}
	rec := c.rec

	subs := make([]chan []byte, len(c.subs))
	copy(subs, c.subs)
	c.mu.Unlock()

	if rec != nil {
		rec.write(frame)
	}
	for _, ch := range subs {
		select {
		case ch <- cp:
		default:
		}
	}
}

func (c *cam) subscribe() chan []byte {
	ch := make(chan []byte, 4)
	c.mu.Lock()
	c.subs = append(c.subs, ch)
	c.mu.Unlock()
	return ch
}

func (c *cam) unsubscribe(ch chan []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, s := range c.subs {
		if s == ch {
			c.subs = append(c.subs[:i], c.subs[i+1:]...)
			return
		}
	}
}

func (c *cam) snap() []byte {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.latest
}

func (c *cam) isOnline() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.online && time.Since(c.lastSeen) < 10*time.Second
}

// ── Cam registry ───────────────────────────────────────────────────────────────

var (
	camsMu sync.RWMutex
	cams   = map[string]*cam{}
)

func getCam(id string) *cam {
	camsMu.RLock()
	c, ok := cams[id]
	camsMu.RUnlock()
	if ok {
		return c
	}
	camsMu.Lock()
	defer camsMu.Unlock()
	if c, ok = cams[id]; ok {
		return c
	}
	c = newCam()
	cams[id] = c
	log.Printf("new camera: %s", id)
	return c
}

func validID(id string) bool {
	return id != "" && !strings.Contains(id, "/") && !strings.Contains(id, ".")
}

// ── Frame ingest ───────────────────────────────────────────────────────────────

func frameHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/frame/")
	if id == "" {
		id = r.Header.Get("X-Cam-ID")
	}
	if !validID(id) {
		http.Error(w, "bad cam id", http.StatusBadRequest)
		return
	}
	frame, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(frame) < 3 {
		http.Error(w, "bad frame", http.StatusBadRequest)
		return
	}
	getCam(id).publish(id, frame)
	w.WriteHeader(http.StatusNoContent)
}

// ── Live MJPEG stream ──────────────────────────────────────────────────────────

const (
	boundary    = "123456789000000000000987654321"
	contentType = "multipart/x-mixed-replace;boundary=" + boundary
)

func liveHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/live/")
	c := getCam(id)

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	ch := c.subscribe()
	defer c.unsubscribe(ch)

	if snap := c.snap(); snap != nil {
		writeFrame(w, snap)
		flusher.Flush()
	}

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-ch:
			if !ok {
				return
			}
			if _, err := writeFrame(w, frame); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeFrame(w io.Writer, frame []byte) (int, error) {
	hdr := fmt.Sprintf("\r\n--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n",
		boundary, len(frame))
	n, err := io.WriteString(w, hdr)
	if err != nil {
		return n, err
	}
	m, err := w.Write(frame)
	return n + m, err
}

// ── Snapshot ───────────────────────────────────────────────────────────────────

func snapHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/snap/")
	frame := getCam(id).snap()
	if frame == nil {
		http.Error(w, "no frame", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(frame)
}

// ── Settings (camera polls GET, UI posts changes) ──────────────────────────────

func cmdHandler(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/cmd/")
	if !validID(id) {
		http.Error(w, "bad cam id", http.StatusBadRequest)
		return
	}
	c := getCam(id)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

	if r.Method == http.MethodOptions {
		return
	}
	if r.Method == http.MethodPost {
		c.mu.RLock()
		s := c.settings
		c.mu.RUnlock()
		if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		clamp := func(v, lo, hi int) int {
			if v < lo {
				return lo
			}
			if v > hi {
				return hi
			}
			return v
		}
		s.Brightness = clamp(s.Brightness, -2, 2)
		s.Contrast = clamp(s.Contrast, -2, 2)
		s.Saturation = clamp(s.Saturation, -2, 2)
		s.AELevel = clamp(s.AELevel, -2, 2)
		s.JpegQuality = clamp(s.JpegQuality, 4, 63)
		c.mu.Lock()
		c.settings = s
		c.mu.Unlock()
		log.Printf("[%s] settings: %+v", id, s)
	}
	c.mu.RLock()
	s := c.settings
	c.mu.RUnlock()
	json.NewEncoder(w).Encode(s)
}

// ── Status ─────────────────────────────────────────────────────────────────────

func statusHandler(w http.ResponseWriter, r *http.Request) {
	camsMu.RLock()
	type entry struct {
		ID     string `json:"id"`
		Online bool   `json:"online"`
	}
	var out []entry
	for id, c := range cams {
		out = append(out, entry{id, c.isOnline()})
	}
	camsMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// ── Auth middleware ────────────────────────────────────────────────────────────

// /frame/ is excluded — camera pushes without auth.
// /live/, /snap/, /cmd/, /status, /rec/, / all require auth.
func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/frame/") {
			next.ServeHTTP(w, r)
			return
		}
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u), []byte(authUser)) == 0 ||
			subtle.ConstantTimeCompare([]byte(p), []byte(authPass)) == 0 {
			w.Header().Set("WWW-Authenticate", `Basic realm="cams"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── Disk cleanup ───────────────────────────────────────────────────────────────

func diskCleanupLoop() {
	for {
		time.Sleep(5 * time.Minute)
		if err := enforceDiskLimit(); err != nil {
			log.Printf("[disk] cleanup error: %v", err)
		}
	}
}

func enforceDiskLimit() error {
	limit := maxDiskGB << 30

	type fileEntry struct {
		path    string
		size    int64
		modTime time.Time
	}
	var files []fileEntry
	var total int64

	err := filepath.WalkDir(recDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		files = append(files, fileEntry{path, info.Size(), info.ModTime()})
		total += info.Size()
		return nil
	})
	if err != nil {
		return err
	}

	if total <= limit {
		return nil
	}

	// sort oldest first
	sort.Slice(files, func(i, j int) bool {
		return files[i].modTime.Before(files[j].modTime)
	})

	for _, f := range files {
		if total <= limit {
			break
		}
		if err := os.Remove(f.path); err == nil {
			log.Printf("[disk] removed %s (%.1f MB)", f.path, float64(f.size)/1e6)
			total -= f.size
		}
	}
	return nil
}

// ── TCP frame ingest ───────────────────────────────────────────────────────────

func startTCPListener() {
	ln, err := net.Listen("tcp", tcpAddr)
	if err != nil {
		log.Fatalf("TCP listen %s: %v", tcpAddr, err)
	}
	log.Printf("TCP listener on %s", tcpAddr)
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("[tcp] accept error: %v", err)
			continue
		}
		go handleTCPCam(conn)
	}
}

func handleTCPCam(conn net.Conn) {
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))

	// first line: camera id
	scanner := bufio.NewReader(conn)
	idLine, err := scanner.ReadString('\n')
	if err != nil {
		log.Printf("[tcp] read id: %v", err)
		return
	}
	id := strings.TrimSpace(idLine)
	if !validID(id) {
		log.Printf("[tcp] bad cam id: %q", id)
		return
	}
	log.Printf("[tcp] camera connected: %s from %s", id, conn.RemoteAddr())

	c := getCam(id)
	var lenBuf [4]byte

	for {
		// no deadline while streaming — just SO_SNDTIMEO on the cam side
		conn.SetReadDeadline(time.Now().Add(15 * time.Second))

		if _, err := io.ReadFull(scanner, lenBuf[:]); err != nil {
			log.Printf("[%s] TCP read len: %v", id, err)
			return
		}
		frameLen := binary.BigEndian.Uint32(lenBuf[:])
		if frameLen == 0 || frameLen > 1<<20 {
			log.Printf("[%s] bad frame len: %d", id, frameLen)
			return
		}
		frame := make([]byte, frameLen)
		if _, err := io.ReadFull(scanner, frame); err != nil {
			log.Printf("[%s] TCP read frame: %v", id, err)
			return
		}
		c.publish(id, frame)
	}
}

// ── Static UI ──────────────────────────────────────────────────────────────────

//go:embed static
var staticFS embed.FS

// ── Main ───────────────────────────────────────────────────────────────────────

func main() {
	os.MkdirAll(recDir, 0o755)

	go diskCleanupLoop()
	go startTCPListener()

	mux := http.NewServeMux()
	mux.HandleFunc("/frame/", frameHandler)
	mux.HandleFunc("/live/", liveHandler)
	mux.HandleFunc("/snap/", snapHandler)
	mux.HandleFunc("/cmd/", cmdHandler)
	mux.HandleFunc("/status", statusHandler)
	mux.Handle("/rec/", http.StripPrefix("/rec/", http.FileServer(http.Dir(recDir))))
	mux.Handle("/", http.FileServer(http.FS(staticFS)))

	log.Printf("listening on %s (user=%s, disk limit=%dGB)", listenAddr, authUser, maxDiskGB)
	log.Fatal(http.ListenAndServe(listenAddr, authMiddleware(mux)))
}
