package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"orbsleep/internal/dockerx"
	"orbsleep/internal/model"
	"orbsleep/internal/scan"
)

// Scan inspects every container (running or not) on the requested environment
// and classifies it. envID empty scans ALL enabled environments and merges the
// results (each container carries its EnvironmentID).
func (e *Engine) Scan(ctx context.Context, envID string) (model.ScanResult, error) {
	if envID == "" {
		return e.scanAll(ctx)
	}
	env, ok, err := e.store.GetEnvironment(envID)
	if err != nil {
		return model.ScanResult{}, err
	}
	if !ok {
		return model.ScanResult{}, fmt.Errorf("环境 %s 不存在", envID)
	}
	return e.scanOne(ctx, env)
}

// scanOne scans a single environment.
func (e *Engine) scanOne(ctx context.Context, env model.Environment) (model.ScanResult, error) {
	c, err := e.clientForEnv(ctx, env.ID)
	if err != nil {
		e.setLastError("扫描失败: " + err.Error())
		return model.ScanResult{}, err
	}
	summaries, err := c.ListContainers(ctx)
	if err != nil {
		if env.ID == e.currentEnvID() {
			e.invalidateDocker()
		} else {
			e.dropEnvClient(env.ID)
		}
		e.setLastError("读取容器列表失败: " + err.Error())
		return model.ScanResult{}, err
	}
	opts := scan.Options{
		ExcludeImagePatterns: e.settings.Get().ExcludeImagePatterns,
		ProxyContainerName:   e.proxySpecForEnv(env.ID).ContainerName,
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
			if cont.Name == opts.ProxyContainerName || cont.ComposeProject == "orbsleep-proxy" || strings.HasPrefix(cont.ComposeProject, "orbsleep-proxy-") {
				// OrbSleep's own proxy is infrastructure, not a candidate.
				return
			}
			if sid, ok := managedByID[cont.ID]; ok {
				cont.ManagedServiceID = sid
			} else if sid, ok := managedByName[cont.Name]; ok {
				cont.ManagedServiceID = sid
			}
			cont.EnvironmentID = env.ID
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
		ScannedAt:     time.Now().UTC().Format(time.RFC3339),
		Context:       c.Context(),
		Containers:    containers,
		Counts:        counts,
		EnvironmentID: env.ID,
	}
	if env.ID == e.currentEnvID() || env.ID == "" {
		e.mu.Lock()
		e.scanCache = containers
		e.scanAt = time.Now()
		e.mu.Unlock()
	}
	e.logger.Printf("info", "扫描完成（%s）: %d 个容器（可接入 %d，需确认 %d，排除 %d）",
		env.Name, len(containers), counts["eligible"], counts["needsReview"], counts["excluded"])
	return res, nil
}

// scanAll scans every enabled environment in parallel and merges results.
func (e *Engine) scanAll(ctx context.Context) (model.ScanResult, error) {
	e.ensurePrimaryEnvironment()
	envs, err := e.store.ListEnvironments()
	if err != nil {
		return model.ScanResult{}, err
	}
	var enabled []model.Environment
	for _, env := range envs {
		if env.Enabled {
			enabled = append(enabled, env)
		}
	}
	if len(enabled) == 0 {
		// Fall back to the primary detection path so a fresh install still
		// scans before the environment row exists.
		env := model.Environment{ID: e.currentEnvID(), Name: "Docker", Kind: "docker-engine"}
		return e.scanOne(ctx, env)
	}
	merged := model.ScanResult{
		ScannedAt:  time.Now().UTC().Format(time.RFC3339),
		Containers: []model.Container{},
		Counts:     map[string]int{"eligible": 0, "needsReview": 0, "excluded": 0, "managed": 0},
	}
	var mu sync.Mutex
	var firstErr error
	var wg sync.WaitGroup
	for _, env := range enabled {
		wg.Add(1)
		go func(env model.Environment) {
			defer wg.Done()
			res, err := e.scanOne(ctx, env)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			merged.Containers = append(merged.Containers, res.Containers...)
			merged.ScannedAt = res.ScannedAt
			if env.ID == e.currentEnvID() || merged.Context.Endpoint == "" {
				merged.Context = res.Context
			}
			for k, v := range res.Counts {
				if k == "managed" {
					continue
				}
				merged.Counts[k] += v
			}
		}(env)
	}
	wg.Wait()
	services, _ := e.store.ListServices()
	merged.Counts["managed"] = len(services)
	// Keep a deterministic order: by environment then name.
	sort.Slice(merged.Containers, func(i, j int) bool {
		a, b := merged.Containers[i], merged.Containers[j]
		if a.EnvironmentID != b.EnvironmentID {
			return a.EnvironmentID < b.EnvironmentID
		}
		if a.ComposeProject != b.ComposeProject {
			return a.ComposeProject < b.ComposeProject
		}
		return a.Name < b.Name
	})
	if firstErr != nil && len(merged.Containers) == 0 {
		return model.ScanResult{}, firstErr
	}
	if firstErr != nil {
		e.logger.Printf("warn", "多环境扫描部分失败: %v", firstErr)
	}
	return merged, nil
}

// CachedScan returns the last primary-environment scan result.
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

// Inspect returns one container plus a redacted raw inspect view. The
// container is looked up on the primary environment first, then on the other
// enabled ones.
func (e *Engine) Inspect(ctx context.Context, id string) (model.Container, map[string]any, error) {
	var lastErr error
	tryClient := func(c *dockerx.Client, envID string) (model.Container, map[string]any, bool, error) {
		ins, err := c.InspectContainer(ctx, id)
		if err != nil {
			if dockerx.IsErrNotFound(err) {
				return model.Container{}, nil, false, nil
			}
			return model.Container{}, nil, false, err
		}
		cont, safe := e.buildInspectView(ins, envID)
		return cont, safe, true, nil
	}
	primary, perr := e.dockerClient(ctx)
	if perr == nil {
		if cont, safe, ok, err := tryClient(primary, e.currentEnvID()); ok || (err != nil) {
			return cont, safe, err
		}
	} else {
		lastErr = perr
	}
	envs, _ := e.store.ListEnvironments()
	for _, env := range envs {
		if !env.Enabled || env.ID == e.currentEnvID() {
			continue
		}
		c, cerr := e.clientForEnv(ctx, env.ID)
		if cerr != nil {
			continue
		}
		if cont, safe, ok, err := tryClient(c, env.ID); ok || (err != nil) {
			return cont, safe, err
		}
	}
	if lastErr != nil {
		return model.Container{}, nil, lastErr
	}
	return model.Container{}, nil, fmt.Errorf("容器 %s 不存在于任何已启用环境", shortID(id))
}

func (e *Engine) buildInspectView(ins *dockerx.ContainerInspect, envID string) (model.Container, map[string]any) {
	cont := scan.Convert(ins, scan.Options{
		ExcludeImagePatterns: e.settings.Get().ExcludeImagePatterns,
		ProxyContainerName:   e.proxySpecForEnv(envID).ContainerName,
	})
	cont.EnvironmentID = envID
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
		"strategy":       cont.Strategy,
		"environmentId":  envID,
	}
	return cont, safe
}
