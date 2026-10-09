// Package secure 提供口令散列。
//
// 存储格式与 Node 版 src/utils/password.js 完全相同：
//
//	scrypt$N$r$p$salt(base64)$hash(base64)
//
// 因此两版后端共用同一个数据库文件时可以互相校验通过。
package secure

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"strconv"
	"strings"

	"golang.org/x/crypto/scrypt"
)

const (
	scryptN      = 16384
	scryptR      = 8
	scryptP      = 1
	scryptKeyLen = 64
	saltLen      = 16
)

// Hash 生成口令散列。
func Hash(password string) string {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}

	key, err := scrypt.Key([]byte(password), salt, scryptN, scryptR, scryptP, scryptKeyLen)
	if err != nil {
		panic(err)
	}

	return strings.Join([]string{
		"scrypt",
		strconv.Itoa(scryptN),
		strconv.Itoa(scryptR),
		strconv.Itoa(scryptP),
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(key),
	}, "$")
}

// Verify 校验口令是否与存储的散列匹配，全程使用常数时间比较。
func Verify(password, stored string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 6 || parts[0] != "scrypt" {
		return false
	}

	n, err1 := strconv.Atoi(parts[1])
	r, err2 := strconv.Atoi(parts[2])
	p, err3 := strconv.Atoi(parts[3])
	if err1 != nil || err2 != nil || err3 != nil {
		return false
	}

	salt, err := base64.StdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) == 0 {
		return false
	}
	expected, err := base64.StdEncoding.DecodeString(parts[5])
	if err != nil || len(expected) == 0 {
		return false
	}

	actual, err := scrypt.Key([]byte(password), salt, n, r, p, len(expected))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(actual, expected) == 1
}
