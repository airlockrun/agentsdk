package localruntime

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// StorageHandler serves an explicit app-local store using the SDK wire protocol.
func StorageHandler(store *FileStorage, op string) http.Handler {
	if store == nil {
		panic("localruntime: storage is required")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { storageRequest(store, w, r, op) })
}

func storageRequest(store *FileStorage, w http.ResponseWriter, r *http.Request, op string) bool {
	var err error
	switch op {
	case "put":
		_, err = store.Write(r.Context(), r.PathValue("key"), r.Body, r.Header.Get("Content-Type"))
	case "get":
		var f io.ReadCloser
		f, err = store.Open(r.Context(), r.PathValue("key"))
		if err == nil {
			defer f.Close()
			info, e := store.Stat(r.Context(), r.PathValue("key"))
			if e != nil {
				err = e
				break
			}
			w.Header().Set("Content-Type", info.ContentType)
			if header := r.Header.Get("Range"); header != "" {
				var start, end int64
				parts := strings.Split(strings.TrimPrefix(header, "bytes="), "-")
				if !strings.HasPrefix(header, "bytes=") || len(parts) != 2 {
					err = errors.New("invalid range")
					break
				}
				start, err = strconv.ParseInt(parts[0], 10, 64)
				if err != nil {
					break
				}
				end, err = strconv.ParseInt(parts[1], 10, 64)
				if err != nil {
					break
				}
				if start < 0 || end < start || start >= info.Size {
					w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
					return true
				}
				end = min(end, info.Size-1)
				if _, err = io.CopyN(io.Discard, f, start); err != nil {
					break
				}
				w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(start, 10)+"-"+strconv.FormatInt(end, 10)+"/"+strconv.FormatInt(info.Size, 10))
				w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
				w.WriteHeader(http.StatusPartialContent)
				_, _ = io.CopyN(w, f, end-start+1)
				return true
			}
			w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
			_, _ = io.Copy(w, f)
			return true
		}
	case "delete":
		err = store.Delete(r.Context(), r.PathValue("key"))
	case "list":
		var files any
		files, err = store.List(r.Context(), r.URL.Query().Get("path"), r.URL.Query().Get("recursive") == "true")
		if err == nil {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(files)
			return true
		}
	case "info":
		var args struct {
			Path string `json:"path"`
		}
		if err = strictJSON(r.Body, &args); err == nil {
			var info any
			info, err = store.Stat(r.Context(), args.Path)
			if err == nil {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(info)
				return true
			}
		}
	case "copy":
		var args struct {
			Src string `json:"src"`
			Dst string `json:"dst"`
		}
		if err = strictJSON(r.Body, &args); err == nil {
			err = store.Copy(r.Context(), args.Src, args.Dst)
		}
	default:
		panic("mockairlock: unknown storage operation")
	}
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		http.Error(w, storageError(err).Error(), status)
	} else {
		w.WriteHeader(http.StatusNoContent)
	}
	return true
}
