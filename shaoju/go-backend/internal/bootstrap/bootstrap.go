// Package bootstrap 启动引导：确保存在管理员账号、清理过期会话、补齐历史数据。
package bootstrap

import (
	"fmt"

	"lostfound/internal/config"
	"lostfound/internal/store"
)

// AdminResult 描述默认管理员的处理结果。
type AdminResult struct {
	Created   bool
	StudentID string
	Password  string
}

// EnsureDefaultAdmin 保证默认管理员存在且具备 admin 角色。
func EnsureDefaultAdmin(s *store.Store, cfg config.Config) (*AdminResult, error) {
	seed := cfg.DefaultAdmin

	existing, err := s.FindUserByStudentID(seed.StudentID)
	if err != nil {
		return nil, err
	}

	if existing != nil {
		if existing.Role != "admin" {
			if err := s.EnsureActiveAdminRole(existing.ID); err != nil {
				return nil, err
			}
		}
		return &AdminResult{Created: false, StudentID: seed.StudentID}, nil
	}

	if _, err := s.CreateUser(seed.StudentID, seed.Password, seed.Name, "", "admin"); err != nil {
		return nil, err
	}
	return &AdminResult{Created: true, StudentID: seed.StudentID, Password: seed.Password}, nil
}

// Run 执行完整引导流程。
func Run(s *store.Store, cfg config.Config) (*AdminResult, error) {
	admin, err := EnsureDefaultAdmin(s, cfg)
	if err != nil {
		return nil, err
	}

	// Node 版后端创建的用户只有 username / nickname，这里补成 student_id / name，
	// 让他们也能用学号登录 Go 版。
	fixed, err := s.BackfillIdentity()
	if err != nil {
		return nil, err
	}
	if fixed > 0 {
		fmt.Printf("[bootstrap] 已为 %d 个历史账号补齐学号\n", fixed)
	}

	removed, err := s.CleanupExpired()
	if err != nil {
		return nil, err
	}
	if removed > 0 {
		fmt.Printf("[bootstrap] 已清理 %d 条过期会话\n", removed)
	}
	return admin, nil
}
