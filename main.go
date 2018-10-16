package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/pprof"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/gorilla/mux"
	"github.com/urfave/negroni"
)

const temp string = "data"

var (
	mu   sync.Mutex
	addr string
)

func init() {
	const (
		defaultPort = ":9999"
		portUsage   = "define TCP port to bind listen on"
	)
	flag.StringVar(&addr, "port", defaultPort, portUsage)
	flag.StringVar(&addr, "p", defaultPort, portUsage+" (shorthand)")
}

func getChunkName(filename string, chunkNbr int) string {
	return fmt.Sprintf("%s_part_%03d", filename, chunkNbr)
}

func serveHome(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "./static/home.html")
}

func resumable(w http.ResponseWriter, r *http.Request) {
	// resumable.js uses a GET request to check if it uploaded the file already
	identifier := r.URL.Query().Get("resumableIdentifier")
	filename := r.URL.Query().Get("resumableFilename")
	chunkStr := r.URL.Query().Get("resumableChunkNumber")

	// TODO: Handle missing query string params

	chunk, err := strconv.Atoi(chunkStr)
	if err != nil {
		log.Printf("error converting chunk: %v to int\n", chunk)
		http.Error(w, "Bad chunk.", http.StatusBadRequest)
		return
	}

	tempDir := filepath.Join(temp, identifier)
	chunkName := getChunkName(filename, chunk)
	chunkFile := filepath.Join(tempDir, chunkName)

	if _, err := os.Stat(chunkFile); os.IsNotExist(err) {
		http.Error(w, "Chunk not found.", http.StatusNotFound)
		return
	}
}

func resumablePost(w http.ResponseWriter, r *http.Request) {
	var err error
	identifier := r.URL.Query().Get("resumableIdentifier")
	filename := r.URL.Query().Get("resumableFilename")
	relativePath := r.URL.Query().Get("resumableRelativePath")
	// chunkSize := r.URL.Query().Get("resumableChunkSize")
	chunkStr := r.URL.Query().Get("resumableChunkNumber")
	totalStr := r.URL.Query().Get("resumableTotalChunks")

	// convert query string attributes to integers
	chunk, err := strconv.Atoi(chunkStr)
	if err != nil {
		log.Printf("error converting chunk: %v to int\n", chunk)
		http.Error(w, "Bad chunk.", http.StatusBadRequest)
		return
	}

	total, err := strconv.Atoi(totalStr)
	if err != nil {
		log.Printf("error converting total: %v to int\n", total)
		http.Error(w, "Bad chunk.", http.StatusBadRequest)
		return
	}

	// get request data from body
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Printf("Error reading body: %v\n", err)
		http.Error(w, "Can't read body", http.StatusBadRequest)
		return
	}

	// make temp dir for storing chunks (if not exists)
	tempDir := filepath.Join(temp, identifier)
	err = os.MkdirAll(tempDir, 0770)
	if err != nil {
		log.Printf("Error creating temp dir: %v\n", err)
		http.Error(w, "Filesystem error", http.StatusInternalServerError)
		return
	}

	// get chunk file name
	chunkName := getChunkName(filename, chunk)

	chunkFile := filepath.Join(tempDir, chunkName)

	// save the chunk data
	err = ioutil.WriteFile(chunkFile, body, 0770)
	if err != nil {
		log.Printf("Error writing file: %v\n", err)
		http.Error(w, "Filesystem error", http.StatusInternalServerError)
		return
	}

	mu.Lock() // wait for mutex so this doesn't get complicated
	defer mu.Unlock()

	// create an array of paths (so we can re-use it later)
	chunkPaths := make([]string, total)
	for i := 0; i < total; i++ {
		chunkPaths[i] = filepath.Join(tempDir, getChunkName(filename, i+1))
	}

	// check if we have all the chunks
	complete := true
	for _, path := range chunkPaths {
		if _, err = os.Stat(path); os.IsNotExist(err) {
			complete = false
			break
		}
	}

	// TODO: Something to prevent second upload with the same filename
	// overwriting the first file uploaded..
	if complete {
		// if so, concat chunks into single file
		// that file should be at relativePath location
		// create any dirs that we need

		target := filepath.Join(temp, relativePath)
		// NOTE: Make sure the parent directories exist for target file
		err = os.MkdirAll(filepath.Dir(target), 0770)
		if err != nil {
			log.Printf("Error creating directory: %v\n", err)
			http.Error(w, "Filesystem error", http.StatusInternalServerError)
			return
		}

		// open the target file for writing output
		// out, err := os.OpenFile(target, os.O_WRONLY|os.O_EXCL, 0770)
		out, err := os.Create(target)
		if err != nil {
			log.Printf("Error file: %v\n", err)
			http.Error(w, "Filesystem error", http.StatusInternalServerError)
			return
		}

		// for each chunk file
		for _, path := range chunkPaths {
			in, err := os.Open(path)
			if err != nil {
				log.Printf("Error reading chunk: %v\n", err)
				http.Error(w, "Filesystem error", http.StatusInternalServerError)
				return
			}
			if _, err = io.Copy(out, in); err != nil {
				log.Printf("Error copying chunk: %v\n", err)
				http.Error(w, "Filesystem error", http.StatusInternalServerError)
				return
			}
			err = in.Close()
			if err != nil {
				log.Printf("Error closing chunk file: %v\n", err)
			}

			// TODO: unlink chunk file
			err = os.Remove(path)
			if err != nil {
				log.Printf("Error deleting chunk: %v\n", err)
				http.Error(w, "Filesystem error", http.StatusInternalServerError)
				return
			}
		}

		/*
			err = out.Sync()
			if err != nil {
				log.Printf("Error flushing file contents to disk: %v\n", err)
				http.Error(w, "Filesystem error", http.StatusInternalServerError)
				return
			}
		*/

		err = out.Close()
		if err != nil {
			log.Printf("Error closing file: %v\n", err)
			http.Error(w, "Filesystem error", http.StatusInternalServerError)
			return

		}

		// remove the temp directory and all chunks
		err = os.RemoveAll(tempDir)
		if err != nil {
			log.Printf("Error removing temp dir: %v\n", err)
			http.Error(w, "Filesystem error", http.StatusInternalServerError)
			return
		}
	}
}

////////////////////////////////////////////////////////////////////////////////
// Logger
////////////////////////////////////////////////////////////////////////////////

// LoggerDefaultFormat is a sane default logger format.
var LoggerDefaultFormat = "{{.StartTime}} | {{.Status}} | {{.Duration}} | {{.Remote}} | {{.Method}} {{.Path}} \n"

// LoggerDefaultDateFormat is my preferred default datetime logger format.
var LoggerDefaultDateFormat = time.RFC3339

// LoggerEntry is the structure passed to the template.
type LoggerEntry struct {
	StartTime string
	Status    int
	Duration  time.Duration
	Remote    string
	Method    string
	Path      string
	Request   *http.Request
}

// ALogger interface implements common fmt functions.
type ALogger interface {
	Print(v ...interface{})
	Println(v ...interface{})
	Printf(format string, v ...interface{})
}

// Logger structure holds http logger specifics.
type Logger struct {
	ALogger
	dateFormat string
	template   *template.Template
}

// NewLogger creates a new logger instance.
func NewLogger() *Logger {
	logger := &Logger{ALogger: log.New(os.Stdout, "", 0), dateFormat: LoggerDefaultDateFormat}
	logger.SetFormat(LoggerDefaultFormat)
	return logger
}

// SetFormat sets template format for logger.
func (l *Logger) SetFormat(format string) {
	l.template = template.Must(template.New("request_parser").Parse(format))
}

// SetDateFormat sets the datetime format for logging representation.
func (l *Logger) SetDateFormat(format string) {
	l.dateFormat = format
}

func (l *Logger) ServeHTTP(w http.ResponseWriter, r *http.Request, next http.HandlerFunc) {
	start := time.Now()

	// TODO: Make this an option?
	// if debug { dump }
	// dump, err := httputil.DumpRequest(r, true)
	// if err != nil {
	// 	log.Println(err)
	// }
	// log.Printf("\n%s\n", string(dump))

	next(w, r)

	unescapedPath, err := url.PathUnescape(r.URL.String())
	if err != nil {
		l.Println(err)
	}

	res := w.(negroni.ResponseWriter)
	thisLog := LoggerEntry{
		StartTime: start.Format(l.dateFormat),
		Status:    res.Status(),
		Duration:  time.Since(start),
		Remote:    r.RemoteAddr,
		Method:    r.Method,
		Path:      unescapedPath,
		// r.URL.String() would be fine here, but unescaping the path will
		// print it out as /api/search?q=test test test instead of printing out
		// /api/search?q=test%20test%20test
		// I'm not sure which is preferable.
		Request: r,
	}

	buff := &bytes.Buffer{}
	err = l.template.Execute(buff, thisLog)
	if err != nil {
		l.Println(err)
	}
	// l.Printf(buff.String()) // BUG: Do not use printf here. The string has
	// already been formatted, don't interpret escape characters as formatting
	// directives. This causes weird (MISSING) messages to be printed in the
	// server logs.
	l.Print(buff.String())
}

////////////////////////////////////////////////////////////////////////////////
////////////////////////////////////////////////////////////////////////////////

func main() {
	flag.Parse()
	if !strings.Contains(addr, ":") {
		addr = ":" + addr
	}

	r := mux.NewRouter()
	r.HandleFunc("/", serveHome).Methods("GET")
	r.HandleFunc("/upload", resumable).Methods("GET")
	r.HandleFunc("/upload", resumablePost).Methods("POST")
	r.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.Dir("./static/"))))

	// Add the pprof routes
	r.HandleFunc("/debug/pprof/", pprof.Index)
	r.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	r.HandleFunc("/debug/pprof/profile", pprof.Profile)
	r.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	r.HandleFunc("/debug/pprof/trace", pprof.Trace)
	r.Handle("/debug/pprof/block", pprof.Handler("block"))
	r.Handle("/debug/pprof/goroutine", pprof.Handler("goroutine"))
	r.Handle("/debug/pprof/heap", pprof.Handler("heap"))
	r.Handle("/debug/pprof/threadcreate", pprof.Handler("threadcreate"))

	n := negroni.New()
	l := NewLogger()
	n.Use(l)
	n.UseHandler(r)

	srv := &http.Server{
		Handler:      n,
		Addr:         ":9999",
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 1 * time.Minute,
	}

	signalChannel := make(chan os.Signal, 2)
	signal.Notify(signalChannel, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-signalChannel
		switch sig {
		case os.Interrupt:
			log.Println("SIGINT")
			_ = srv.Shutdown(context.Background())
		case syscall.SIGTERM:
			log.Println("SIGTERM")
			_ = srv.Shutdown(context.Background())
		}
	}()

	log.Printf("Starting server on %s\n", srv.Addr)
	log.Fatal(srv.ListenAndServe())
}
