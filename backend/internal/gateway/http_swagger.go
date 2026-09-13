package gateway

import (
	"embed"
	"net/http"
)

//go:embed swagger/index.html swagger/openapi.json
var swaggerFiles embed.FS

func redirectSwaggerIndex(writer http.ResponseWriter, request *http.Request) {
	http.Redirect(writer, request, "/swagger/", http.StatusFound)
}

func serveSwaggerUI(writer http.ResponseWriter, _ *http.Request) {
	serveSwaggerAsset(writer, "swagger/index.html", "text/html; charset=utf-8")
}

func serveOpenAPISpec(writer http.ResponseWriter, _ *http.Request) {
	serveSwaggerAsset(writer, "swagger/openapi.json", "application/json")
}

func serveSwaggerAsset(writer http.ResponseWriter, name, contentType string) {
	body, err := swaggerFiles.ReadFile(name)
	if err != nil {
		http.Error(writer, "swagger asset missing", http.StatusInternalServerError)
		return
	}
	writer.Header().Set("Content-Type", contentType)
	writer.WriteHeader(http.StatusOK)
	_, _ = writer.Write(body)
}
