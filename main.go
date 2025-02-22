package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
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
			MaxUploadSize = 20480 * 1024 * 1024
		}
	} else {
		MaxUploadSize = 20480 * 1024 * 1024
	}
	if memStr := os.Getenv("MAX_MEMORY"); memStr != "" {
		if mem, err := strconv.ParseInt(memStr, 10, 64); err == nil {
			MaxMemory = mem * 1024 * 1024
		} else {
			log.Printf("Error parsing MAX_MEMORY: %v, using default", err)
			MaxMemory = 256 * 1024 * 1024
		}
	} else {
		MaxMemory = 256 * 1024 * 1024
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

func saveUploadedFile(ctx context.Context, file multipart.File, header *multipart.FileHeader) error {
	defer file.Close()
	if header.Size > MaxUploadSize {
		return fmt.Errorf("файл %s слишком большой", header.Filename)
	}
	tempFileName := fmt.Sprintf("temp_%d_%s", time.Now().UnixNano(), filepath.Base(header.Filename))
	tempFilePath := filepath.Join(TempUploadPath, tempFileName)
	tempFile, err := os.Create(tempFilePath)
	if err != nil {
		return err
	}
	written, err := copyWithContext(ctx, tempFile, file)
	tempFile.Close()
	if err != nil {
		os.Remove(tempFilePath)
		return err
	}
	if written != header.Size {
		os.Remove(tempFilePath)
		return fmt.Errorf("файл %s загрузился не полностью: ожидалось %d байт, записано %d байт", header.Filename, header.Size, written)
	}
	hash, err := randomHash(4)
	if err != nil {
		os.Remove(tempFilePath)
		return err
	}
	timestamp := time.Now().Format("20060102_150405")
	safeName := filepath.Base(header.Filename)
	newFileName := fmt.Sprintf("%s_%s_%s", hash, timestamp, safeName)
	finalPath := filepath.Join(UploadPath, newFileName)
	if err := moveFile(tempFilePath, finalPath); err != nil {
		os.Remove(tempFilePath)
		return err
	}
	return nil
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Метод не поддерживается", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	r.Body = http.MaxBytesReader(w, r.Body, MaxUploadSize*10)
	if err := r.ParseMultipartForm(MaxMemory); err != nil {
		http.Error(w, "Ошибка парсинга формы: "+err.Error(), http.StatusBadRequest)
		return
	}
	files := r.MultipartForm.File["videos"]
	if len(files) == 0 {
		http.Error(w, "Файлы не выбраны", http.StatusBadRequest)
		return
	}
	var wg sync.WaitGroup
	errChan := make(chan error, len(files))
	for _, fileHeader := range files {
		wg.Add(1)
		go func(fh *multipart.FileHeader) {
			defer wg.Done()
			file, err := fh.Open()
			if err != nil {
				errChan <- fmt.Errorf("не удалось открыть файл %s: %v", fh.Filename, err)
				return
			}
			if err := saveUploadedFile(ctx, file, fh); err != nil {
				errChan <- fmt.Errorf("ошибка сохранения файла %s: %v", fh.Filename, err)
				return
			}
		}(fileHeader)
	}
	wg.Wait()
	close(errChan)
	var errorMsg string
	for err := range errChan {
		errorMsg += err.Error() + "\n"
	}
	if errorMsg != "" {
		http.Error(w, "Возникли ошибки:\n"+errorMsg, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	fmt.Fprint(w, `<html>
	<head>
		<meta charset="UTF-8">
		<title>Загрузка завершена</title>
		<link rel="stylesheet" href="https://cdn.jsdelivr.net/npm/bootstrap@5.3.0/dist/css/bootstrap.min.css">
	</head>
	<body class="d-flex justify-content-center align-items-center" style="height: 100vh;">
		<div class="alert alert-success" role="alert">
			Все файлы успешно загружены!
		</div>
	</body>
</html>`)
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
	<style>
		body { background-color: #f8f9fa; }
		.container { max-width: 600px; margin-top: 50px; }
	</style>
</head>
<body>
<div class="container">
	<h2 class="mb-4 text-center">Загрузка видео файлов</h2>
	<form method="POST" action="/upload" enctype="multipart/form-data">
		<div class="mb-3">
			<input class="form-control" type="file" name="videos" multiple accept="video/*">
		</div>
		<button type="submit" class="btn btn-primary w-100">Загрузить</button>
	</form>
</div>
</body>
</html>
	`)
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/", indexHandler)
	mux.HandleFunc("/upload", uploadHandler)
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
