// Command campaign-events receives provider webhook events and serves
// per-campaign statistics. Storage is in memory (lost on restart).
package main

import (
	"flag"
	"log"
	"net/http"
	"time"
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	skew := flag.Duration("max-future-skew", 24*time.Hour,
		"reject events whose timestamp is further than this in the future (0 disables the check)")
	flag.Parse()

	srv := NewServer(NewStore(), time.Now, *skew)
	httpSrv := &http.Server{
		Addr:              *addr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("listening on %s", *addr)
	log.Fatal(httpSrv.ListenAndServe())
}
