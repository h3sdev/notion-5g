// Package web embebe el dashboard estático (HTML/CSS/JS vanilla, sin build
// step ni dependencias externas de CDN propias) en el propio binario del
// servidor, usando embed.FS para no depender de archivos sueltos en el
// filesystem de despliegue.
package web

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed static
var staticFS embed.FS

// Handler sirve el dashboard: static/index.html en "/" y el resto de
// static/** (p. ej. static/assets/*) bajo su misma ruta relativa. No exige
// X-API-Key: la clave se pide dentro de la propia página y se manda como
// header en cada llamada fetch() a /api/v1/*.
func Handler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// No debería ocurrir nunca: "static" está embebido en tiempo de
		// compilación por la directiva go:embed de arriba.
		panic("web: static embebido inválido: " + err.Error())
	}
	fileServer := http.FileServer(http.FS(sub))
	indexHTML := buildIndexHTML(sub)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// El dashboard se sirve detrás de Cloudflare, que cachea agresivamente
		// extensiones como .js/.css en su borde por defecto (ya nos mordió una
		// vez: un 404 de una corrida vieja quedó cacheado 4h hasta purgarlo a
		// mano). Este header ya le pide al origen no cachear, pero Cloudflare
		// no lo respeta para /assets/* -- ver buildIndexHTML() para el arreglo
		// real (cache-busting por contenido), esto queda como red adicional.
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		if r.URL.Path == "/" || r.URL.Path == "/index.html" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write(indexHTML)
			return
		}
		fileServer.ServeHTTP(w, r)
	})
}

// buildIndexHTML lee static/index.html y le agrega "?v=<hash>" a las
// referencias de app.js/style.css, con un hash calculado sobre el contenido
// real de esos archivos en este binario (se computa una sola vez, al armar
// el Handler). Así cada despliegue que cambia el dashboard sirve una URL de
// asset distinta -- tanto el navegador como Cloudflare cachean por URL
// completa, así que una URL nueva se pide siempre fresca al origen, sin
// depender de acordarse de purgar el caché a mano (el gotcha real de §5.3
// del handover). Si el contenido no cambió entre despliegues, el hash da
// igual y no se generan purgas de más.
func buildIndexHTML(sub fs.FS) []byte {
	index, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		panic("web: index.html embebido inválido: " + err.Error())
	}
	v := assetVersion(sub, "assets/app.js", "assets/style.css")
	html := string(index)
	html = strings.ReplaceAll(html, `src="/assets/app.js"`, `src="/assets/app.js?v=`+v+`"`)
	html = strings.ReplaceAll(html, `href="/assets/style.css"`, `href="/assets/style.css?v=`+v+`"`)
	return []byte(html)
}

func assetVersion(sub fs.FS, paths ...string) string {
	h := sha256.New()
	for _, p := range paths {
		b, err := fs.ReadFile(sub, p)
		if err != nil {
			continue // no debería pasar (rutas fijas dentro del propio embed), pero no vale la pena tumbar el server por esto
		}
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:10]
}
