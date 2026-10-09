package main

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrItemNotFound        = errors.New("信息不存在")
	ErrItemForbidden       = errors.New("只能操作自己发布的信息")
	ErrInvalidItemStatus   = errors.New("目标状态只允许 found 或 closed")
	ErrItemStatusConflict  = errors.New("信息已变化或当前状态不允许此操作，请刷新后重试")
	ErrInvalidItemFields   = errors.New("类型、物品名称和地点不能为空")
	ErrInvalidItemPage     = errors.New("page 必须是 1～1000000 的整数")
	ErrInvalidItemPageSize = errors.New("page_size 必须是 1～100 的整数")
)

type validationError string

func (e validationError) Error() string { return string(e) }

type rateLimitError string

func (e rateLimitError) Error() string { return string(e) }

type ItemService struct{ repo *ItemRepository }

func (s *ItemService) checkOwner(ctx context.Context, id, user int) error {
	owner, found, err := s.repo.GetOwnerID(ctx, id)
	if err != nil {
		return err
	}
	if !found {
		return ErrItemNotFound
	}
	if owner != user {
		return ErrItemForbidden
	}
	return nil
}
func (s *ItemService) ChangeStatus(ctx context.Context, id, user int, target string) (Item, error) {
	if err := s.checkOwner(ctx, id, user); err != nil {
		return Item{}, err
	}
	previous := ""
	switch target {
	case "found":
		previous = "searching"
	case "closed":
		previous = "found"
	default:
		return Item{}, ErrInvalidItemStatus
	}
	i, updated, err := s.repo.UpdateStatus(ctx, id, user, previous, target)
	if err != nil {
		return Item{}, err
	}
	if !updated {
		return Item{}, ErrItemStatusConflict
	}
	return i, nil
}
func validateItem(in *CreateItemRequest, creating bool) error {
	in.ItemType = strings.TrimSpace(in.ItemType)
	if in.ItemType != "lost" && in.ItemType != "found" {
		return validationError("type 只允许 lost（丢失帖）或 found（找到帖）")
	}
	for _, f := range []struct {
		v        *string
		name     string
		max      int
		required bool
	}{{&in.Name, "名称", 100, true}, {&in.Location, "地点", 120, true}, {&in.Description, "描述", 2000, false}, {&in.Category, "分类", 32, false}, {&in.Color, "颜色", 32, false}, {&in.Brand, "品牌", 64, false}, {&in.Contact, "联系方式", 200, false}} {
		*f.v = strings.TrimSpace(*f.v)
		if f.required && *f.v == "" {
			return ErrInvalidItemFields
		}
		if utf8.RuneCountInString(*f.v) > f.max {
			return validationError(f.name + "过长")
		}
	}
	if in.OccurredAt < 0 || in.OccurredAt > time.Now().Add(24*time.Hour).Unix() {
		return validationError("事件时间无效")
	}
	if len(in.Tags) > 10 {
		return validationError("标签最多 10 个")
	}
	if in.Tags == nil {
		in.Tags = []string{}
	}
	for j := range in.Tags {
		in.Tags[j] = strings.TrimSpace(in.Tags[j])
		if in.Tags[j] == "" || utf8.RuneCountInString(in.Tags[j]) > 32 {
			return validationError("每个标签需要 1～32 个字符")
		}
	}
	if len(in.ImageIDs) > 3 {
		return validationError("每帖最多 3 张图片")
	}
	seen := map[int]bool{}
	for _, id := range in.ImageIDs {
		if id < 1 || seen[id] {
			return validationError("图片 ID 必须是不同的正整数")
		}
		seen[id] = true
	}
	if in.ItemType == "found" && (creating || in.ImageIDs != nil) && len(in.ImageIDs) == 0 {
		return validationError("找到帖至少需要一张图片")
	}
	return nil
}
func (s *ItemService) Create(ctx context.Context, user int, in CreateItemRequest) (Item, error) {
	if err := validateItem(&in, true); err != nil {
		return Item{}, err
	}
	return s.repo.Create(ctx, user, in)
}
func (s *ItemService) Update(ctx context.Context, id, user int, in CreateItemRequest) (Item, error) {
	if err := s.checkOwner(ctx, id, user); err != nil {
		return Item{}, err
	}
	if err := validateItem(&in, false); err != nil {
		return Item{}, err
	}
	i, ok, err := s.repo.Update(ctx, id, user, in)
	if err != nil {
		return Item{}, err
	}
	if !ok {
		return Item{}, ErrItemNotFound
	}
	return i, nil
}
func (s *ItemService) Delete(ctx context.Context, id, user int) error {
	if err := s.checkOwner(ctx, id, user); err != nil {
		return err
	}
	ok, err := s.repo.Delete(ctx, id, user)
	if err != nil {
		return err
	}
	if !ok {
		return ErrItemNotFound
	}
	return nil
}
func (s *ItemService) GetByID(ctx context.Context, id int) (Item, error) {
	i, ok, err := s.repo.GetByID(ctx, id)
	if err != nil {
		return Item{}, err
	}
	if !ok {
		return Item{}, ErrItemNotFound
	}
	return i, nil
}
func (s *ItemService) Search(ctx context.Context, q SearchQuery) (ItemListResponse, error) {
	if q.Page < 1 || q.Page > 1_000_000 {
		return ItemListResponse{}, ErrInvalidItemPage
	}
	if q.PageSize < 1 || q.PageSize > 100 {
		return ItemListResponse{}, ErrInvalidItemPageSize
	}
	if q.Breadth == "" {
		q.Breadth = "standard"
	}
	if q.Breadth != "strict" && q.Breadth != "standard" && q.Breadth != "broad" {
		return ItemListResponse{}, validationError("breadth 只允许 strict、standard 或 broad")
	}
	if q.ItemType != "" && q.ItemType != "lost" && q.ItemType != "found" {
		return ItemListResponse{}, validationError("type 无效")
	}
	if q.Status != "" && q.Status != "searching" && q.Status != "found" && q.Status != "closed" {
		return ItemListResponse{}, validationError("status 无效")
	}
	q.Keyword = strings.TrimSpace(q.Keyword)
	if utf8.RuneCountInString(q.Keyword) > 100 {
		return ItemListResponse{}, validationError("关键词最多 100 字")
	}
	items, total, err := s.repo.Search(ctx, q)
	return ItemListResponse{Items: items, Total: total, Page: q.Page, PageSize: q.PageSize}, err
}
