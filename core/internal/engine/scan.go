package engine

import (
	"context"
	"sort"
	"sync"
	"time"

	"orbsleep/internal/dockerx"
	"orbsleep/internal/model"
	"orbsleep/internal/proxy"
	"orbsleep/internal/scan"
)

// Scan inspects every container (running or not) and classifies it.
func (e *Engine) Scan(ctx context.Context) (model.ScanResult, error) {
	c, err := e.dockerClient(ctx)
	if err != nil {
		e.setLastError("扫描失败: " + err.Error())
		return model.ScanResult{}, err
	}
	summaries, err := c.ListContainers(ctx)
	if err != nil {
		e.invalidateDocker()
		e.setLastError("读取容器列表失败: " + err.Error())
		return model.ScanResult{}, err
	}
	opts := scan.Options{
		ExcludeImagePatterns: e.settings.Get().ExcludeImagePatterns,
		ProxyContainerName:   proxy.ContainerName,
	}
	services, _ := e.store.ListServices()
	managedByID := map[string]string{}
	managedByName := map[string]string{}
	for _, svc := range services {
		managedByID[svc.ContainerID] = svc.ID
		managedByName[svc.ContainerName] = svc.ID
	}

	results := make([]model.Container, len(summaries))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, sum := range summaries {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ins, err := c.InspectContainer(ctx, id)
			if err != nil {
				if dockerx.IsErrNotFound(err) {
					return
				}
				e.logger.Printf("warn", "inspect %s 失败: %v", id, err)
				return
			}
			cont := scan.Convert(ins, opts)
			if cont.Name == proxy.ContainerName {
				// OrbSleep's own proxy is infrastructure, not a candidate.
				return
			}
			if sid, ok := managedByID[cont.ID]; ok {
				cont.ManagedServiceID = sid
			} else if sid, ok := managedByName[cont.Name]; ok {
				cont.ManagedServiceID = sid
			}
			results[i] = cont
		}(i, sum.ID)
	}
	wg.Wait()

	containers := make([]model.Container, 0, len(results))
	for _, c := range results {
		if c.ID != "" {
			containers = append(containers, c)
		}
	}
	sort.Slice(containers, func(i, j int) bool {
		a, b := containers[i], containers[j]
		if a.ComposeProject != b.ComposeProject {
			return a.ComposeProject < b.ComposeProject
		}
		if a.ComposeService != b.ComposeService {
			return a.ComposeService < b.ComposeService
		}
		return a.Name < b.Name
	})

	counts := map[string]int{"eligible": 0, "needsReview": 0, "excluded": 0, "managed": len(services)}
	for _, cont := range containers {
		switch cont.Classification.Verdict {
		case "eligible":
			counts["eligible"]++
		case "needs_review":
			counts["needsReview"]++
		case "excluded":
			counts["excluded"]++
		}
	}
	if containers == nil {
		containers = []model.Container{}
	}
	res := model.ScanResult{
		ScannedAt:  time.Now().UTC().Format(time.RFC3339),
		Context:    c.Context(),
		Containers: containers,
		Counts:     counts,
	}
	e.mu.Lock()
	e.scanCache = containers
	e.scanAt = time.Now()
	e.mu.Unlock()
	e.logger.Printf("info", "扫描完成: %d 个容器（可接入 %d，需确认 %d，排除 %d）",
		len(containers), counts["eligible"], counts["needsReview"], counts["excluded"])
	return res, nil
}

// CachedScan returns the last scan result without touching Docker.
func (e *Engine) CachedScan() (model.ScanResult, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.scanAt.IsZero() {
		return model.ScanResult{}, false
	}
	res := model.ScanResult{
		ScannedAt:  e.scanAt.UTC().Format(time.RFC3339),
		Containers: e.scanCache,
		Counts:     map[string]int{},
	}
	for _, c := range e.scanCache {
		switch c.Classification.Verdict {
		case "eligible":
			res.Counts["eligible"]++
		case "needs_review":
			res.Counts["needsReview"]++
		case "excluded":
			res.Counts["excluded"]++
		}
	}
	return res, true
}

// Inspect returns one container plus a redacted raw inspect view.
func (e *Engine) Inspect(ctx context.Context, id string) (model.Container, map[string]any, error) {
	c, err := e.dockerClient(ctx)
	if err != nil {
		return model.Container{}, nil, err
	}
	ins, err := c.InspectContainer(ctx, id)
	if err != nil {
		return model.Container{}, nil, err
	}
	cont := scan.Convert(ins, scan.Options{
		ExcludeImagePatterns: e.settings.Get().ExcludeImagePatterns,
		ProxyContainerName:   proxy.ContainerName,
	})
	services, _ := e.store.ListServices()
	for _, svc := range services {
		if svc.ContainerID == cont.ID || svc.ContainerName == cont.Name {
			cont.ManagedServiceID = svc.ID
		}
	}
	envKeys := make([]string, 0, len(ins.Config.Env))
	for _, kv := range ins.Config.Env {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				envKeys = append(envKeys, kv[:i]+"=***")
				break
			}
		}
	}
	networks := map[string]any{}
	for name, ep := range ins.NetworkSettings.Networks {
		networks[name] = map[string]any{"ip": ep.IPAddress, "gateway": ep.Gateway}
	}
	safe := map[string]any{
		"id":             ins.ID,
		"name":           ins.Name,
		"created":        ins.Created,
		"image":          ins.Config.Image,
		"state":          ins.State.Status,
		"running":        ins.State.Running,
		"exitCode":       ins.State.ExitCode,
		"restartPolicy":  ins.HostConfig.RestartPolicy.Name,
		"networkMode":    ins.HostConfig.NetworkMode,
		"autoRemove":     ins.HostConfig.AutoRemove,
		"privileged":     ins.HostConfig.Privileged,
		"readonlyRootfs": ins.HostConfig.ReadonlyRootfs,
		"capAdd":         ins.HostConfig.CapAdd,
		"envKeys":        envKeys,
		"labels":         ins.Config.Labels,
		"networks":       networks,
		"mounts":         ins.Mounts,
		"portBindings":   ins.HostConfig.PortBindings,
		"publishedPorts": ins.NetworkSettings.Ports,
	}
	return cont, safe, nil
}
