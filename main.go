package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"
)

var (
	MaxUploadSize  int64
	MaxMemory      int64
	UploadPath     string
	TempUploadPath string
)

func init() {
	if sizeStr := os.Getenv("MAX_UPLOAD_SIZE"); sizeStr != "" {
		if size, err := strconv.ParseInt(sizeStr, 10, 64); err == nil {
			MaxUploadSize = size * 1024 * 1024
		} else {
			log.Printf("Error parsing MAX_UPLOAD_SIZE: %v, using default", err)
			MaxUploadSize = 10 << 30
		}
	} else {
		MaxUploadSize = 10 << 30
	}
	if memStr := os.Getenv("MAX_MEMORY"); memStr != "" {
		if mem, err := strconv.ParseInt(memStr, 10, 64); err == nil {
			MaxMemory = mem * 1024 * 1024
		} else {
			log.Printf("Error parsing MAX_MEMORY: %v, using default", err)
			MaxMemory = 32 << 20
		}
	} else {
		MaxMemory = 32 << 20
	}
	if path := os.Getenv("UPLOAD_PATH"); path != "" {
		UploadPath = path
	} else {
		UploadPath = "./uploads"
	}
	if tempPath := os.Getenv("TEMP_UPLOAD_PATH"); tempPath != "" {
		TempUploadPath = tempPath
	} else {
		TempUploadPath = "./temp_uploads"
	}
	os.MkdirAll(UploadPath, os.ModePerm)
	os.MkdirAll(TempUploadPath, os.ModePerm)
	files, err := os.ReadDir(TempUploadPath)
	if err == nil {
		for _, f := range files {
			os.RemoveAll(filepath.Join(TempUploadPath, f.Name()))
		}
	}
}

func randomHash(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func copyWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buf := make([]byte, 32*1024)
	var written int64
	for {
		select {
		case <-ctx.Done():
			return written, ctx.Err()
		default:
		}
		n, err := src.Read(buf)
		if n > 0 {
			nw, ew := dst.Write(buf[:n])
			written += int64(nw)
			if ew != nil {
				return written, ew
			}
			if n != nw {
				return written, io.ErrShortWrite
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return written, err
		}
	}
	return written, nil
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

func uploadChunkHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if err := r.ParseMultipartForm(MaxMemory); err != nil {
		http.Error(w, "Error parsing form: "+html.EscapeString(err.Error()), http.StatusBadRequest)
		return
	}
	uploadID := r.FormValue("upload_id")
	chunkIndexStr := r.FormValue("chunk_index")
	totalChunksStr := r.FormValue("total_chunks")
	filename := r.FormValue("filename")
	totalSizeStr := r.FormValue("total_size")
	if uploadID == "" || chunkIndexStr == "" || totalChunksStr == "" || filename == "" || totalSizeStr == "" {
		http.Error(w, "Missing parameters", http.StatusBadRequest)
		return
	}
	chunkIndex, err := strconv.Atoi(chunkIndexStr)
	if err != nil {
		http.Error(w, "Invalid chunk_index", http.StatusBadRequest)
		return
	}
	totalChunks, err := strconv.Atoi(totalChunksStr)
	if err != nil {
		http.Error(w, "Invalid total_chunks", http.StatusBadRequest)
		return
	}
	totalSize, err := strconv.ParseInt(totalSizeStr, 10, 64)
	if err != nil {
		http.Error(w, "Invalid total_size", http.StatusBadRequest)
		return
	}
	chunkFile, _, err := r.FormFile("chunk")
	if err != nil {
		http.Error(w, "Missing file chunk: "+html.EscapeString(err.Error()), http.StatusBadRequest)
		return
	}
	defer chunkFile.Close()
	tempFileName := fmt.Sprintf("%s_%s", uploadID, filepath.Base(filename))
	tempFilePath := filepath.Join(TempUploadPath, tempFileName)
	f, err := os.OpenFile(tempFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		http.Error(w, "Error opening temp file: "+html.EscapeString(err.Error()), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	_, err = copyWithContext(ctx, f, chunkFile)
	if err != nil {
		http.Error(w, "Error writing chunk: "+html.EscapeString(err.Error()), http.StatusInternalServerError)
		return
	}
	info, err := os.Stat(tempFilePath)
	if err != nil {
		http.Error(w, "Error stating temp file: "+html.EscapeString(err.Error()), http.StatusInternalServerError)
		return
	}
	progress := float64(info.Size()) / float64(totalSize) * 100
	w.Header().Set("Content-Type", "text/html")
	if chunkIndex == totalChunks-1 {
		if info.Size() != totalSize {
			os.Remove(tempFilePath)
			http.Error(w, "File incomplete, expected size mismatch", http.StatusInternalServerError)
			return
		}
		hash, err := randomHash(4)
		if err != nil {
			os.Remove(tempFilePath)
			http.Error(w, "Error generating hash: "+html.EscapeString(err.Error()), http.StatusInternalServerError)
			return
		}
		timestamp := time.Now().Format("20060102_150405")
		newFileName := fmt.Sprintf("%s_%s_%s", hash, timestamp, filepath.Base(filename))
		finalPath := filepath.Join(UploadPath, newFileName)
		if err := moveFile(tempFilePath, finalPath); err != nil {
			os.Remove(tempFilePath)
			http.Error(w, "Error moving file: "+html.EscapeString(err.Error()), http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(w, `<div class="text-success">Файл %s загружен успешно!</div>`, html.EscapeString(filename))
		return
	}
	fmt.Fprintf(w, `<div>Чанк %d из %d получен. Прогресс: %.2f%%</div>`, chunkIndex+1, totalChunks, progress)
}

func indexHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, `
<!DOCTYPE html>
<html lang="ru">
<head>
	<meta charset="UTF-8">
	<title>Загрузка видео файлов</title>
	<link href="https://cdn.jsdelivr.net/npm/bootstrap@5.3.0/dist/css/bootstrap.min.css" rel="stylesheet">
	<script src="https://unpkg.com/htmx.org@1.9.2"></script>
	<style>
		body { background-color: #f8f9fa; }
		.container { max-width: 700px; margin-top: 50px; }
		.progress { margin-top: 5px; height: 25px; }
	</style>
</head>
<body>
<div class="container">
	<h2 class="mb-4 text-center">Загрузка видео файлов</h2>
	<form id="uploadForm">
		<div class="mb-3">
			<input class="form-control" type="file" id="videos" name="videos" multiple accept="video/*">
		</div>
		<button type="submit" class="btn btn-primary w-100">Начать загрузку</button>
	</form>
	<hr/>
	<div id="message"></div>
</div>
<script>
function uploadFile(file) {
	return new Promise((resolve, reject) => {
		var chunkSize = 1024 * 1024;
		var totalChunks = Math.ceil(file.size / chunkSize);
		var uploadID = 'upload_' + Math.random().toString(36).substr(2, 9);
		var container = document.createElement("div");
		container.innerHTML = '<strong>' + file.name + '</strong>: <span id="status_' + uploadID + '">0%</span>';
		var progressDiv = document.createElement("div");
		progressDiv.className = "progress mb-2";
		progressDiv.innerHTML = '<div id="progress_' + uploadID + '" class="progress-bar" role="progressbar" style="width: 0%;">0%</div>';
		var fileBlock = document.createElement("div");
		fileBlock.appendChild(container);
		fileBlock.appendChild(progressDiv);
		document.getElementById("message").appendChild(fileBlock);
		function uploadChunk(chunkIndex) {
			var start = chunkIndex * chunkSize;
			var end = Math.min(start + chunkSize, file.size);
			var chunk = file.slice(start, end);
			var formData = new FormData();
			formData.append('upload_id', uploadID);
			formData.append('chunk_index', chunkIndex);
			formData.append('total_chunks', totalChunks);
			formData.append('filename', file.name);
			formData.append('total_size', file.size);
			formData.append('chunk', chunk);
			var xhr = new XMLHttpRequest();
			xhr.open('POST', '/upload_chunk', true);
			xhr.onload = function() {
				if(xhr.status === 200) {
					document.getElementById("progress_" + uploadID).innerHTML = xhr.responseText;
					if(xhr.responseText.indexOf("успешно") !== -1) {
						document.getElementById("status_" + uploadID).textContent = "Завершено";
						resolve();
					} else {
						document.getElementById("status_" + uploadID).textContent = "Чанк " + (chunkIndex+1) + " из " + totalChunks;
						uploadChunk(chunkIndex + 1);
					}
				} else {
					document.getElementById("status_" + uploadID).textContent = "Ошибка: " + xhr.responseText;
					reject(xhr.responseText);
				}
			};
			xhr.onerror = function() {
				document.getElementById("status_" + uploadID).textContent = "Ошибка загрузки чанка.";
				reject("XHR error");
			};
			xhr.send(formData);
		}
		uploadChunk(0);
	});
}

document.getElementById('uploadForm').addEventListener('submit', function(e) {
	e.preventDefault();
	document.getElementById("message").innerHTML = "";
	var files = document.getElementById('videos').files;
	if(files.length === 0){
		alert('Выберите хотя бы один файл.');
		return;
	}
	var promises = [];
	for(var i = 0; i < files.length; i++){
		promises.push(uploadFile(files[i]));
	}
	Promise.all(promises)
	.then(function(){
		htmx.alert("Все файлы успешно загружены!");
	})
	.catch(function(err){
		htmx.alert("Ошибка загрузки: " + err);
	});
});
</script>
</body>
</html>
	`)
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/upload_chunk", uploadChunkHandler)
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
