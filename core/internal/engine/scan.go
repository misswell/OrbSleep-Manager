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
	"orbsleep/internal/runtime"
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
	return e.scanOne(ctx, env, nil)
}

// scanOne scans a single environment. globalSem (optional) caps total
// concurrent inspects across a multi-environment scan: each environment has
// its own per-scan limiter, and without a global cap N environments would
// yield N×8 concurrent API calls against possibly resource-constrained VMs.
func (e *Engine) scanOne(ctx context.Context, env model.Environment, globalSem chan struct{}) (model.ScanResult, error) {
	rt, err := e.runtimeForEnv(ctx, env.ID)
	if err != nil {
		e.setLastError("扫描失败: " + err.Error())
		return model.ScanResult{}, err
	}
	summaries, err := rt.ListContainers(ctx)
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
	// The managed index decides whether an already-managed container shows up
	// as managed or as an ordinary attachable candidate. An unknown board is
	// not a clean board: refuse to scan rather than mislead the operator.
	services, err := e.store.ListServices()
	if err != nil {
		return model.ScanResult{}, fmt.Errorf("读取托管服务失败，无法安全扫描: %w", err)
	}
	primary := e.currentEnvID()
	// Both matching maps are scoped to THIS environment: scanOne only ever
	// inspects containers of one engine, so a service pinned elsewhere can
	// never legitimately match here. This keeps the identity model at
	// (environment, container id/name) instead of trusting ID uniqueness
	// across engines.
	managedByID := map[string]string{}
	managedByName := map[string]string{}
	managedInEnv := 0
	for _, svc := range services {
		svcEnv := svc.EnvironmentID
		if svcEnv == "" {
			if svc.DockerSocket == "" || svc.DockerSocket == strings.TrimPrefix(e.currentEnvEndpoint(), "unix://") {
				svcEnv = primary
			} else {
				svcEnv = runtime.EnvID("unix://" + svc.DockerSocket)
			}
		}
		if svcEnv != env.ID {
			continue
		}
		managedInEnv++
		if svc.ContainerID != "" {
			managedByID[svc.ContainerID] = svc.ID
		}
		if svc.ContainerName != "" {
			managedByName[svc.ContainerName] = svc.ID
		}
	}

	type scanItem struct {
		cont model.Container
		fail *model.ScanContainerFailure
	}
	results := make([]scanItem, len(summaries))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, sum := range summaries {
		cname := ""
		if len(sum.Names) > 0 {
			cname = strings.TrimPrefix(sum.Names[0], "/")
		}
		wg.Add(1)
		go func(i int, id string, cname string) {
			defer wg.Done()
			if globalSem != nil {
				globalSem <- struct{}{}
				defer func() { <-globalSem }()
			}
			sem <- struct{}{}
			defer func() { <-sem }()
			ins, err := rt.InspectContainer(ctx, id)
			if err != nil {
				if dockerx.IsErrNotFound(err) {
					// The container vanished mid-scan: a normal race, treated
					// as absent.
					return
				}
				// Any other failure means this environment's scan is
				// incomplete — record it so Partial stays honest.
				results[i] = scanItem{fail: &model.ScanContainerFailure{
					EnvironmentID: env.ID,
					ContainerID:   id,
					ContainerName: cname,
					Error:         err.Error(),
				}}
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
			results[i] = scanItem{cont: cont}
		}(i, sum.ID, cname)
	}
	wg.Wait()

	containers := make([]model.Container, 0, len(results))
	failedContainers := []model.ScanContainerFailure{}
	partial := false
	for _, item := range results {
		if item.fail != nil {
			failedContainers = append(failedContainers, *item.fail)
			partial = true
			continue
		}
		if item.cont.ID != "" {
			containers = append(containers, item.cont)
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

	counts := map[string]int{"eligible": 0, "needsReview": 0, "excluded": 0, "managed": managedInEnv}
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
	info := rt.Info()
	res := model.ScanResult{
		ScannedAt: time.Now().UTC().Format(time.RFC3339),
		Context: model.ContextInfo{
			Name:     info.Name,
			Endpoint: info.Endpoint,
			Kind:     string(info.Kind),
		},
		Containers:       containers,
		Counts:           counts,
		EnvironmentID:    env.ID,
		Partial:          partial,
		FailedContainers: failedContainers,
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
		return e.scanOne(ctx, env, nil)
	}
	merged := model.ScanResult{
		ScannedAt:  time.Now().UTC().Format(time.RFC3339),
		Containers: []model.Container{},
		Counts:     map[string]int{"eligible": 0, "needsReview": 0, "excluded": 0, "managed": 0},
	}
	var mu sync.Mutex
	var firstErr error
	// One global inspect cap across all environments: a per-env limiter alone
	// would scale concurrency with the environment count.
	globalSem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for _, env := range enabled {
		wg.Add(1)
		go func(env model.Environment) {
			defer wg.Done()
			res, err := e.scanOne(ctx, env, globalSem)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				// A failed environment must never masquerade as a complete
				// scan: record it explicitly so the GUI can say "partial".
				merged.Partial = true
				merged.FailedEnvironments = append(merged.FailedEnvironments, model.ScanEnvironmentFailure{
					EnvironmentID:   env.ID,
					EnvironmentName: env.Name,
					Error:           err.Error(),
				})
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
				merged.Counts[k] += v
			}
		}(env)
	}
	wg.Wait()
	// managed counts were summed per scanned environment in the merge loop
	// (the "managed" key is no longer skipped there); a global len(services)
	// would count services of disabled environments that were never scanned.
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
	tryRuntime := func(rt runtime.Runtime, envID string) (model.Container, map[string]any, bool, error) {
		ins, err := rt.InspectContainer(ctx, id)
		if err != nil {
			if dockerx.IsErrNotFound(err) {
				return model.Container{}, nil, false, nil
			}
			return model.Container{}, nil, false, err
		}
		cont, safe := e.buildInspectView(ins, envID)
		return cont, safe, true, nil
	}
	primaryRT, perr := e.runtimeForEnv(ctx, e.currentEnvID())
	if perr == nil {
		if cont, safe, ok, err := tryRuntime(primaryRT, e.currentEnvID()); ok || (err != nil) {
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
		rt, cerr := e.runtimeForEnv(ctx, env.ID)
		if cerr != nil {
			continue
		}
		if cont, safe, ok, err := tryRuntime(rt, env.ID); ok || (err != nil) {
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
