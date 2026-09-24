// Command service is the web service the demo publishes through a tunnel. It
// answers every path with one page, which is what the recording opens on the
// far side of the Host.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"
)

const page = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Demo service</title>
<style>
  body { margin: 0; min-height: 100vh; display: flex; align-items: center; justify-content: center;
         background: #0f1720; color: #e6edf3; font-family: system-ui, sans-serif; }
  main { text-align: center; padding: 48px 64px; border: 1px solid #2f3b47; border-radius: 16px;
         background: #16212c; }
  h1 { margin: 0 0 16px; font-size: 40px; }
  p { margin: 8px 0; font-size: 20px; color: #9fb0c0; }
  code { color: #7ee0a1; }
</style>
</head>
<body>
<main>
<h1>Hello from the service behind the tunnel</h1>
<p>This page is served on the machine tunnel-manager runs on.</p>
<p>It was reached at <code id="address"></code></p>
</main>
<script>document.getElementById("address").textContent = window.location.href;</script>
</body>
</html>
`

func main() {
	listen := flag.String("listen", "127.0.0.1:8000", "address to listen on")
	flag.Parse()

	server := &http.Server{
		Addr: *listen,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(page))
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("demo service listening on %s", *listen)
	log.Fatal(server.ListenAndServe())
}
