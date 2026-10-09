package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
)

type ItemHandler struct {
	service *ItemService
}

func (h *ItemHandler) ChangeStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPatch {
		w.Header().Set("Allow", "PATCH")
		writeJSON(w, http.StatusMethodNotAllowed, Response{
			Code:    405,
			Message: "只允许 PATCH 请求",
		})
		return
	}

	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, Response{
			Code:    400,
			Message: "ID 必须是正整数",
		})
		return
	}

	user, ok := requireLogin(w, r)
	if !ok {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 10*1024)

	var input UpdateItemStatusRequest
	err = json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, Response{
			Code:    400,
			Message: "请求 JSON 过大或无效",
		})
		return
	}

	item, err := h.service.ChangeStatus(
		r.Context(),
		id,
		user.ID,
		input.Status,
	)

	if err != nil {
		writeItemError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Code:    0,
		Message: "修改状态成功",
		Data:    item,
	})
}

func writeItemError(w http.ResponseWriter, err error) {
	status := http.StatusInternalServerError
	message := "物品操作失败"

	var invalid validationError
	var limited rateLimitError
	switch {
	case errors.As(err, &limited):
		status = http.StatusTooManyRequests
		message = limited.Error()
	case errors.As(err, &invalid):
		status = http.StatusBadRequest
		message = invalid.Error()
	case errors.Is(err, ErrInvalidMedia):
		status = http.StatusBadRequest
		message = ErrInvalidMedia.Error()
	case errors.Is(err, ErrItemNotFound):
		status = http.StatusNotFound
		message = ErrItemNotFound.Error()
	case errors.Is(err, ErrItemForbidden):
		status = http.StatusForbidden
		message = ErrItemForbidden.Error()
	case errors.Is(err, ErrInvalidItemFields):
		status = http.StatusBadRequest
		message = ErrInvalidItemFields.Error()
	case errors.Is(err, ErrInvalidItemStatus):
		status = http.StatusBadRequest
		message = ErrInvalidItemStatus.Error()
	case errors.Is(err, ErrItemStatusConflict):
		status = http.StatusConflict
		message = ErrItemStatusConflict.Error()
	case errors.Is(err, ErrInvalidItemPage):
		status = http.StatusBadRequest
		message = ErrInvalidItemPage.Error()
	case errors.Is(err, ErrInvalidItemPageSize):
		status = http.StatusBadRequest
		message = ErrInvalidItemPageSize.Error()
	default:
		log.Println("物品操作失败：", err)
	}

	writeJSON(w, status, Response{
		Code:    status,
		Message: message,
	})
}

func (h *ItemHandler) Update(
	w http.ResponseWriter,
	r *http.Request,
	itemID int,
	userID int,
) {
	var input CreateItemRequest
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, Response{
			Code:    400,
			Message: "请求内容不是正确的 JSON",
		})
		return
	}

	item, err := h.service.Update(r.Context(), itemID, userID, input)
	if err != nil {
		writeItemError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Code:    0,
		Message: "修改成功",
		Data:    item,
	})
}

func (h *ItemHandler) Delete(
	w http.ResponseWriter,
	r *http.Request,
	itemID int,
	userID int,
) {
	err := h.service.Delete(r.Context(), itemID, userID)
	if err != nil {
		writeItemError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Code:    0,
		Message: "删除成功",
	})
}

func (h *ItemHandler) ByID(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		writeJSON(w, http.StatusBadRequest, Response{
			Code:    400,
			Message: "ID 必须是正整数",
		})
		return
	}

	switch r.Method {
	case http.MethodGet:
		h.Get(w, r, id)

	case http.MethodPut, http.MethodDelete:
		user, ok := requireLogin(w, r)
		if !ok {
			return
		}

		if r.Method == http.MethodPut {
			h.Update(w, r, id, user.ID)
		} else {
			h.Delete(w, r, id, user.ID)
		}

	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, Response{
			Code:    405,
			Message: "不支持这个请求方式",
		})
	}
}

func (h *ItemHandler) Create(
	w http.ResponseWriter,
	r *http.Request,
	userID int,
) {
	var input CreateItemRequest
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	err := json.NewDecoder(r.Body).Decode(&input)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, Response{
			Code:    400,
			Message: "请求内容不是正确的 JSON",
		})
		return
	}

	item, err := h.service.Create(r.Context(), userID, input)
	if err != nil {
		writeItemError(w, err)
		return
	}

	writeJSON(w, http.StatusCreated, Response{
		Code:    0,
		Message: "发布成功",
		Data:    item,
	})
}

func (h *ItemHandler) Get(
	w http.ResponseWriter,
	r *http.Request,
	itemID int,
) {
	item, err := h.service.GetByID(r.Context(), itemID)
	if err != nil {
		writeItemError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if _, err := currentUser(r); err != nil {
		item.Contact = ""
	}

	writeJSON(w, http.StatusOK, Response{
		Code:    0,
		Message: "查询成功",
		Data:    item,
	})
}

func (h *ItemHandler) Collection(
	w http.ResponseWriter,
	r *http.Request,
) {
	switch r.Method {
	case http.MethodGet:
		h.List(w, r)

	case http.MethodPost:
		user, ok := requireLogin(w, r)
		if !ok {
			return
		}

		h.Create(w, r, user.ID)

	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusMethodNotAllowed, Response{
			Code:    405,
			Message: "只允许 GET 或 POST 请求",
		})
	}
}

func (h *ItemHandler) List(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	keyword := params.Get("q")

	page := 1
	pageSize := 20

	if raw := params.Get("page"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			writeItemError(w, ErrInvalidItemPage)
			return
		}
		page = value
	}

	if raw := params.Get("page_size"); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil {
			writeItemError(w, ErrInvalidItemPageSize)
			return
		}
		pageSize = value
	}

	result, err := h.service.Search(r.Context(), SearchQuery{
		Keyword: keyword, Breadth: params.Get("breadth"), ItemType: params.Get("type"),
		Status: params.Get("status"), Page: page, PageSize: pageSize,
	})
	if err != nil {
		writeItemError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, Response{
		Code:    0,
		Message: "查询成功",
		Data:    result,
	})
}
