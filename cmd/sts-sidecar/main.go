package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/GateNLP/minio-sts-k8s/pkg/sidecar"
	"go.uber.org/zap"
)

const (
	ProbePath = "/probe"
)

func main() {
	port := os.Getenv("SIDECAR_PORT")
	if port == "" {
		port = "8787"
	}

	if len(os.Args) >= 2 && os.Args[1] == "probe" {
		// we're running the startup probe rather than the actual sidecar
		os.Exit(probe(port))
	}

	_, debugMode := os.LookupEnv("SIDECAR_DEBUG")

	var logger *zap.Logger
	if debugMode {
		logger = zap.Must(zap.NewDevelopment())
	} else {
		logger = zap.Must(zap.NewProduction())
	}
	defer logger.Sync() // flushes buffer, if any
	sugar := logger.Sugar()

	sugar.Infow("Starting credentials sidecar", "port", port)
	router := http.NewServeMux()
	router.Handle("/creds", sidecar.New(sugar))
	// endpoint for use by the startup probe
	router.Handle(ProbePath, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	// normally we *want* the sidecar to listen only on loopback, since
	// if it were listening on all interfaces then *any* other code in
	// the whole cluster would be able to impersonate this pod's service
	// account when talking to Minio.  But for things like debugging the
	// docker build, it is occasionally useful to listen on all interfaces
	listenAddr := "127.0.0.1"
	if debugMode {
		listenAddr = ""
	}
	server := http.Server{
		Addr:    fmt.Sprintf("%s:%s", listenAddr, port),
		Handler: router,
	}

	sugar.Fatal(server.ListenAndServe())
}

func probe(port string) int {
	c := http.Client{Timeout: time.Second}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%s%s", port, ProbePath))
	if err != nil {
		// failed to connect -> server is not ready
		return 1
	}
	defer func() {
		_, _ = io.ReadAll(resp.Body)
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 400 {
		// server is up
		return 0
	}
	// server is not yet ready
	return 1
}
