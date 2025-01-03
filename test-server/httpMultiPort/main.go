package main

import (
	"fmt"
	"log"
	"net/http"
	"sync"
)

func startServer(port int, wg *sync.WaitGroup) {
	defer wg.Done()

	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		message := fmt.Sprintf("%d Port Connected Successfully\n", port)
		fmt.Fprintf(w, message)
		log.Printf("Request received on port %d", port)
	})

	addr := fmt.Sprintf(":%d", port)
	log.Printf("Starting server on port %d", port)

	server := &http.Server{
		Addr:    addr,
		Handler: mux,
	}

	err := server.ListenAndServe()
	if err != nil {
		log.Printf("Error starting server on port %d: %v", port, err)
	}
}

func main() {
	ports := []int{2000, 2001, 2002}

	var wg sync.WaitGroup

	for _, port := range ports {
		wg.Add(1)
		go startServer(port, &wg)
	}

	log.Println("All servers starting. Press Ctrl+C to stop.")
	wg.Wait()
}
