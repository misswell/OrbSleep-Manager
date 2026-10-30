package engine

import (
	"errors"
	"fmt"
	"os"

	"orbsleep/internal/compose"
	"orbsleep/internal/model"
)

// DoctorVerify adds deep consistency checks to the basic doctor report.
func (e *Engine) DoctorVerify() []model.DoctorCheck {
	var checks []model.DoctorCheck
	add := func(id, title, status, detail, remediation string) {
		checks = append(checks, model.DoctorCheck{ID: id, Title: title, Status: status, Detail: detail, Remediation: remediation})
	}
	txns, err := e.store.ListTransactions()
	if err != nil {
		add("transactions", "未完成事务", "fail", "读取事务失败: "+err.Error(), "备份数据库后报告问题")
	} else {
		active := 0
		for _, t := range txns {
			switch t.Status {
			case "running", "rollback_failed", "needs_repair":
				active++
			}
		}
		if active > 0 {
			add("transactions", "未完成事务", "warn", fmt.Sprintf("%d 个事务需要恢复或人工处理", active),
				"运行 orbsleepctl recover；无法恢复时保留备份并导出诊断")
		} else {
			add("transactions", "未完成事务", "ok", "没有未完成事务", "")
		}
	}

	services, err := e.store.ListServices()
	if err != nil {
		add("compose_state", "Compose 文件一致性", "fail", "读取服务失败: "+err.Error(), "")
		return checks
	}
	if len(services) == 0 {
		add("compose_state", "Compose 文件一致性", "ok", "没有托管服务", "")
	} else {
		issueCount := 0
		for _, svc := range services {
			backupDir, ok := e.store.GetMeta("serviceBackup:" + svc.ID)
			if !ok {
				issueCount++
				add("backup:"+svc.ID, "服务备份 "+svc.ContainerName, "warn", "缺少 serviceBackup 索引", "检查备份目录；不要删除事务记录")
			} else if _, serr := os.Stat(backupDir); serr != nil {
				issueCount++
				add("backup:"+svc.ID, "服务备份 "+svc.ContainerName, "fail", "备份目录不可用: "+serr.Error(), "从 Time Machine 或诊断包恢复")
			}
			snap, serr := e.composeSnapshotStore().Load(composeSnapshotID(svc.ComposeFile))
			if serr != nil {
				issueCount++
				add("compose:"+svc.ID, "Compose 快照 "+svc.ContainerName, "warn", "缺少文件级快照: "+serr.Error(), "运行 recover 前先备份现场")
				continue
			}
			if _, ferr := os.Stat(svc.ComposeFile); errors.Is(ferr, os.ErrNotExist) {
				issueCount++
				add("compose:"+svc.ID, "Compose 文件 "+svc.ContainerName, "fail", "文件已丢失", "使用最后可信快照恢复；不要重建空白文件")
				continue
			} else if ferr != nil {
				issueCount++
				add("compose:"+svc.ID, "Compose 文件 "+svc.ContainerName, "fail", "读取失败: "+ferr.Error(), "检查权限")
				continue
			}
			fp, ferr := composeFingerprint(svc.ComposeFile)
			if ferr != nil {
				issueCount++
				add("compose:"+svc.ID, "Compose 指纹 "+svc.ContainerName, "warn", ferr.Error(), "")
			} else if !fp.SameContent(snap.LastKnownGoodFingerprint) {
				issueCount++
				add("compose:"+svc.ID, "Compose 指纹 "+svc.ContainerName, "warn", "当前内容与最后可信快照不一致（可能是外部修改）",
					"不要自动覆盖；确认外部修改后在界面上重新扫描")
			}
		}
		if issueCount == 0 {
			add("compose_state", "Compose 文件一致性", "ok", "服务备份与最后可信快照完整", "")
		}
	}

	routes := e.gw.Snapshot()
	expected := 0
	for _, svc := range services {
		expected += len(svc.Bindings)
	}
	if len(routes) != expected {
		add("gateway", "网关路由一致性", "warn", fmt.Sprintf("路由 %d 条，期望 %d 条", len(routes), expected),
			"运行 doctor 后检查孤立路由；reconcile 会清理失效路由")
	} else {
		add("gateway", "网关路由一致性", "ok", fmt.Sprintf("%d 条路由均由托管服务驱动", len(routes)), "")
	}
	return checks
}

func composeFingerprint(path string) (compose.FileFingerprint, error) {
	return compose.FingerprintPath(path)
}
