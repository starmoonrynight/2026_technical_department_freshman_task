package main

import (
	"sort"
	"strings"
)

type searchTerm struct {
	text   string
	weight int
}

// Deterministic expansion avoids an AI charge on every search or page request.
var synonymGroups = [][]string{
	{"校园卡", "学生卡", "一卡通", "饭卡", "学生证"},
	{"耳机", "蓝牙耳机", "无线耳机", "airpods", "earbuds"},
	{"手机", "智能手机", "iphone", "phone"},
	{"钥匙", "钥匙串", "门禁钥匙"},
	{"钱包", "钱夹", "卡包"},
	{"水杯", "保温杯", "杯子", "水瓶"},
	{"雨伞", "折叠伞", "遮阳伞"},
	{"眼镜", "近视镜", "墨镜"},
	{"背包", "书包", "双肩包"},
}
var categoryGroups = map[string][]string{
	"卡片":   {"校园卡", "学生卡", "一卡通", "学生证", "银行卡", "证件"},
	"电子产品": {"耳机", "手机", "平板", "电脑", "充电器", "充电宝", "电子产品"},
	"日用品":  {"水杯", "雨伞", "眼镜", "日用品"},
	"包袋":   {"钱包", "背包", "卡包", "书包", "包袋"},
	"钥匙":   {"钥匙", "门禁卡", "钥匙串"},
}

func expandSearch(keyword, breadth string) []searchTerm {
	keyword = strings.ToLower(strings.TrimSpace(keyword))
	if keyword == "" {
		return nil
	}
	terms := []searchTerm{{keyword, 12}}
	seen := map[string]bool{keyword: true}
	add := func(text string, weight int) {
		text = strings.ToLower(text)
		if !seen[text] && len(terms) < 32 {
			seen[text] = true
			terms = append(terms, searchTerm{text, weight})
		}
	}
	if breadth == "strict" {
		return terms
	}
	for _, part := range strings.Fields(keyword) {
		add(part, 6)
	}
	for _, group := range synonymGroups {
		hit := false
		for _, term := range group {
			if strings.Contains(keyword, term) {
				hit = true
			}
		}
		if hit {
			for _, term := range group {
				add(term, 5)
			}
		}
	}
	if breadth == "broad" {
		keys := []string{}
		for k := range categoryGroups {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, category := range keys {
			group := categoryGroups[category]
			hit := strings.Contains(keyword, category)
			for _, term := range group {
				if strings.Contains(keyword, term) {
					hit = true
				}
			}
			if hit {
				add(category, 2)
				for _, term := range group {
					add(term, 2)
				}
			}
		}
	}
	return terms
}
