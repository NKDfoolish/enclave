// Command fakeprovider is a development tool: an OpenAI-format chat provider that answers
// instantly with a fixed streamed answer. Benchmarks use it so they measure the gateway's
// own overhead, not how fast a model generates. Never deploy it.
package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
)

// chunks is how many text chunks each answer has, like a short reply of ~20 tokens.
const chunks = 20

func main() {
	addr := ":8099"
	if v := os.Getenv("FAKE_PROVIDER_ADDR"); v != "" {
		addr = v
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", streamAnswer)
	server := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("fake provider listening on %s", addr)
	log.Fatal(server.ListenAndServe())
}

func streamAnswer(w http.ResponseWriter, _ *http.Request) {
	flusher := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	for i := range chunks {
		_, _ = fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":\"word%d \"},\"finish_reason\":null}]}\n\n", i)
		flusher.Flush()
	}
	_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	_, _ = fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":%d}}\n\n", chunks)
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	flusher.Flush()
}
