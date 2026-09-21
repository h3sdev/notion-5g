// Command server levanta el backend de ingestión para las pruebas de campo del
// módem Notion 5G: recibe mediciones (operador, banda, señal, velocidad,
// ubicación) desde notion5g.py o desde la futura app Android/Flutter, y las
// deja consultables por HTTP.
package main

import (
	"log"
	"net/http"
	"os"
	"time"

	"notion5g/server/internal/api"
	"notion5g/server/internal/store"
)

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	dbPath := getenv("DB_PATH", "/data/notion5g.db")
	addr := getenv("LISTEN_ADDR", ":8080")
	apiKey := os.Getenv("API_KEY")

	if apiKey == "" {
		log.Println("ADVERTENCIA: API_KEY vacía, el servidor acepta escrituras sin autenticar (solo para desarrollo)")
	}

	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("no se pudo abrir la base de datos (%s): %v", dbPath, err)
	}
	defer st.Close()

	srv := api.New(st, apiKey)
	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           srv,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("notion5g-server escuchando en %s (db=%s, auth=%v)", addr, dbPath, apiKey != "")
	if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("servidor caído: %v", err)
	}
}
