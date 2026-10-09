package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, Response{
			Code:    405,
			Message: "只允许 POST 请求",
		})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 10*1024)

	var input RegisterRequest

	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, Response{
			Code:    400,
			Message: "请求 JSON 无效或过大",
		})
		return
	}

	input.Username = strings.TrimSpace(input.Username)

	if input.Username == "" || len(input.Username) > 32 {
		writeJSON(w, http.StatusBadRequest, Response{
			Code:    400,
			Message: "用户名需要为 1~32 字节",
		})
		return
	}

	if len(input.Password) < 8 || len(input.Password) > 72 {
		writeJSON(w, http.StatusBadRequest, Response{
			Code:    400,
			Message: "密码需要为 8~72 字节",
		})
		return
	}

	hash, err := bcrypt.GenerateFromPassword(
		[]byte(input.Password),
		bcrypt.DefaultCost,
	)
	if err != nil {
		log.Println("生成密码哈希失败：", err)
		writeJSON(w, http.StatusInternalServerError, Response{
			Code:    500,
			Message: "注册失败",
		})
		return
	}

	var user User

	err = db.QueryRow(`
		INSERT INTO users (username, password_hash)
		VALUES (?, ?)
		ON CONFLICT(username) DO NOTHING
		RETURNING id, username
	`,
		input.Username,
		string(hash),
	).Scan(
		&user.ID,
		&user.Username,
	)

	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusConflict, Response{
			Code:    409,
			Message: "用户名已存在",
		})
		return
	}

	if err != nil {
		log.Println("保存用户失败", err)
		writeJSON(w, http.StatusInternalServerError, Response{
			Code:    500,
			Message: "注册失败",
		})
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Code:    0,
		Message: "注册成功",
		Data:    user,
	})
}

func handleLogin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, Response{
			Code:    405,
			Message: "只允许 POST 请求",
		})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 10*1024)

	var input LoginRequest
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, Response{
			Code:    400,
			Message: "请求 JSON 无效或过大",
		})
		return
	}

	input.Username = strings.TrimSpace(input.Username)

	if input.Username == "" || len(input.Username) > 32 || len(input.Password) < 1 || len(input.Password) > 72 {
		writeJSON(w, http.StatusBadRequest, Response{
			Code:    400,
			Message: "用户名或密码格式不正确",
		})
		return
	}

	var user User
	var passwordHash string

	err = db.QueryRow(`
		SELECT id, username, password_hash
		FROM users
		WHERE username = ?
	`, input.Username).Scan(
		&user.ID,
		&user.Username,
		&passwordHash,
	)

	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusUnauthorized, Response{
			Code:    401,
			Message: "用户名或密码错误",
		})
		return
	}

	if err != nil {
		log.Println("查询登录用户失败：", err)
		writeJSON(w, http.StatusInternalServerError, Response{
			Code:    500,
			Message: "登录失败",
		})
		return
	}

	err = bcrypt.CompareHashAndPassword(
		[]byte(passwordHash),
		[]byte(input.Password),
	)
	if err == bcrypt.ErrMismatchedHashAndPassword {
		writeJSON(w, http.StatusUnauthorized, Response{
			Code:    401,
			Message: "用户名或密码错误",
		})
		return
	}

	if err != nil {
		log.Println("验证密码哈希失败：", err)
		writeJSON(w, http.StatusInternalServerError, Response{
			Code:    500,
			Message: "登录失败",
		})
		return
	}

	token := rand.Text()
	expireAt := time.Now().Add(24 * time.Hour)

	_, err = db.Exec(`
		INSERT INTO sessions (token, user_id, expires_at)
		VALUES (?, ?, ?)
	`, token, user.ID, expireAt.Unix())
	if err != nil {
		log.Println("保存登录状态失败：", err)
		writeJSON(w, http.StatusInternalServerError, Response{
			Code:    500,
			Message: "登陆失败",
		})
		return
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil || envOr("APP_COOKIE_SECURE", "false") == "true",
		Expires:  expireAt,
		MaxAge:   24 * 60 * 60,
	})

	writeJSON(w, http.StatusOK, Response{
		Code:    0,
		Message: "登录成功",
		Data:    user,
	})
}

func handleLogout(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, Response{
			Code:    405,
			Message: "只允许 POST 请求",
		})
		return
	}

	cookie, err := r.Cookie("session")
	if err == nil {
		_, err = db.Exec(
			"DELETE FROM sessions WHERE token = ?",
			cookie.Value,
		)
		if err != nil {
			log.Println("删除登录状态失败：", err)
			writeJSON(w, http.StatusInternalServerError, Response{
				Code:    500,
				Message: "退出登录失败",
			})
			return
		}
	}

	http.SetCookie(w, &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		Secure:   r.TLS != nil || envOr("APP_COOKIE_SECURE", "false") == "true",
		MaxAge:   -1,
		Expires:  time.Unix(1, 0),
	})

	writeJSON(w, http.StatusOK, Response{
		Code:    0,
		Message: "已退出登录",
	})
}

func handleMe(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")

	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, Response{
			Code:    405,
			Message: "只允许 GET 请求",
		})
		return
	}

	cookie, err := r.Cookie("session")
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, Response{
			Code:    401,
			Message: "请先登录",
		})
		return
	}

	var user User

	err = db.QueryRow(`
		SELECT users.id, users.username
		FROM sessions
		JOIN users ON users.id = sessions.user_id
		WHERE sessions.token = ?
			AND sessions.expires_at > ?
	`, cookie.Value, time.Now().Unix()).Scan(
		&user.ID,
		&user.Username,
	)

	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusUnauthorized, Response{
			Code:    401,
			Message: "登陆状态无效或已过期",
		})
		return
	}

	if err != nil {
		log.Println("查询当前用户失败：", err)
		writeJSON(w, http.StatusInternalServerError, Response{
			Code:    500,
			Message: "查询当前用户失败",
		})
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Code:    0,
		Message: "查询成功",
		Data:    user,
	})
}

func requireLogin(w http.ResponseWriter, r *http.Request) (User, bool) {
	cookie, err := r.Cookie("session")
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, Response{
			Code:    401,
			Message: "请先登录",
		})
		return User{}, false
	}

	var user User

	err = db.QueryRow(`
		SELECT users.id, users.username
		FROM sessions
		JOIN users ON users.id = sessions.user_id
		WHERE sessions.token = ?
		  AND sessions.expires_at > ?
	`, cookie.Value, time.Now().Unix()).Scan(
		&user.ID,
		&user.Username,
	)

	if err == sql.ErrNoRows {
		writeJSON(w, http.StatusUnauthorized, Response{
			Code:    401,
			Message: "登录状态无效或已过期",
		})
		return User{}, false
	}

	if err != nil {
		log.Println("检查登录状态失败：", err)
		writeJSON(w, http.StatusInternalServerError, Response{
			Code:    500,
			Message: "检查登录状态失败",
		})
		return User{}, false
	}

	return user, true
}
