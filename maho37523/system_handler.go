package main

import "net/http"

func handleHome(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "只允许 GET 请求", http.StatusMethodNotAllowed)
		return
	}

	w.Write([]byte("欢迎使用校园失物招领系统"))
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "只允许 GET 请求", http.StatusMethodNotAllowed)
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Code:    0,
		Message: "服务器运行正常",
	})
}
