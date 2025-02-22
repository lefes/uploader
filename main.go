package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tus/tusd/v2/pkg/filelocker"
	"github.com/tus/tusd/v2/pkg/filestore"
	tusd "github.com/tus/tusd/v2/pkg/handler"
)

var (
	UploadPath     string
	TempUploadPath string
)

func init() {
	if path := os.Getenv("UPLOAD_PATH"); path != "" {
		UploadPath = path
	} else {
		UploadPath = "./uploads"
	}
	if tempPath := os.Getenv("TEMP_UPLOAD_PATH"); tempPath != "" {
		TempUploadPath = tempPath
	} else {
		TempUploadPath = "./tusdata"
	}
	os.MkdirAll(UploadPath, os.ModePerm)
	os.MkdirAll(TempUploadPath, os.ModePerm)
}

func moveFile(src, dst string) error {
	err := os.Rename(src, dst)
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.EXDEV) {
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer out.Close()
		if _, err = io.Copy(out, in); err != nil {
			return err
		}
		return os.Remove(src)
	}
	return err
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	htmlStr := `<!DOCTYPE html>
<html lang="ru">
<head>
  <meta charset="UTF-8">
  <title>TUS Upload</title>
  <link href="https://cdn.jsdelivr.net/npm/bootstrap@5.3.0/dist/css/bootstrap.min.css" rel="stylesheet">
</head>
<body>
<div class="container mt-5">
  <h2>Загрузка файлов через TUS</h2>
  <input type="file" id="fileInput" multiple accept="video/*" class="form-control" />
  <button id="uploadBtn" class="btn btn-primary mt-3">Загрузить</button>
  <div id="progress" class="progress mt-3" style="display:none;">
    <div id="progressBar" class="progress-bar" role="progressbar" style="width: 0%;">0%</div>
  </div>
  <div id="status" class="mt-3"></div>
</div>
<script src="https://unpkg.com/tus-js-client/dist/tus.js"></script>
<script>
document.getElementById('uploadBtn').addEventListener('click', function() {
    var files = document.getElementById('fileInput').files;
    if(files.length === 0){
        alert("Выберите файл(ы) для загрузки.");
        return;
    }
    for(var i = 0; i < files.length; i++){
        uploadFile(files[i]);
    }
});
function uploadFile(file){
    var upload = new tus.Upload(file, {
        endpoint: window.location.origin + "/files/",
        retryDelays: [0, 1000, 3000, 5000],
        metadata: {
            filename: file.name,
            filetype: file.type
        },
        onError: function(error){
            document.getElementById('status').innerHTML += "<div class='alert alert-danger'>Ошибка: " + error + "</div>";
        },
        onProgress: function(bytesUploaded, bytesTotal){
            var percentage = (bytesUploaded / bytesTotal * 100).toFixed(2);
            document.getElementById('progress').style.display = 'block';
            document.getElementById('progressBar').style.width = percentage + "%";
            document.getElementById('progressBar').textContent = percentage + "%";
        },
        onSuccess: function(){
            document.getElementById('status').innerHTML += "<div class='alert alert-success'>Файл " + file.name + " загружен успешно!</div>";
        }
    });
    upload.start();
}
</script>
</body>
</html>`
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, htmlStr)
}

func main() {
	store := filestore.New(TempUploadPath)
	locker := filelocker.New(TempUploadPath)
	composer := tusd.NewStoreComposer()
	store.UseIn(composer)
	locker.UseIn(composer)

	config := tusd.Config{
		BasePath:              "/files/",
		StoreComposer:         composer,
		NotifyCompleteUploads: true,
		DisableDownload:       true, // можно изменить по необходимости
		MaxSize:               0,    // лимита нет
		Cors: &tusd.CorsConfig{
			Disable: true,
		},
	}
	tusHandler, err := tusd.NewHandler(config)
	if err != nil {
		log.Fatalf("Unable to create tus handler: %s", err.Error())
	}

	go func() {
		for {
			event := <-tusHandler.CompleteUploads
			log.Printf("Upload %s finished", event.Upload.ID)
			srcPath := filepath.Join(TempUploadPath, event.Upload.ID)
			origName := event.Upload.MetaData["filename"]
			if origName == "" {
				origName = "file"
			}
			newFileName := fmt.Sprintf("%s_%s", time.Now().Format("20060102_150405"), origName)
			dstPath := filepath.Join(UploadPath, newFileName)
			if err := moveFile(srcPath, dstPath); err != nil {
				log.Printf("Error moving file: %s", err.Error())
			} else {
				log.Printf("File moved to %s", dstPath)
			}
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler)
	mux.Handle("/files/", http.StripPrefix("/files/", tusHandler))

	srv := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	idleConnsClosed := make(chan struct{})
	go func() {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Interrupt, syscall.SIGTERM)
		<-sigint
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
		close(idleConnsClosed)
	}()

	log.Println("Server started on :8080")
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		log.Fatalf("ListenAndServe: %v", err)
	}
	<-idleConnsClosed
	log.Println("Server shutdown gracefully")
}
