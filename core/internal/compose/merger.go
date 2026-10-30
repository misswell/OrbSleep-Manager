package compose

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"orbsleep/internal/model"
)

// RestoreManagedBindings safely adds this service's TCP mappings without
// replacing the rest of the current file. It refuses on host-port conflicts,
// missing services, or declarations it cannot parse.
func RestoreManagedBindings(current []byte, service string, bindings []model.PortBinding) ([]byte, []error) {
	if _, err := Services(current); err != nil {
		return nil, []error{err}
	}
	if !ServiceExists(current, service) {
		return nil, []error{fmt.Errorf("%w: %s", ErrServiceNotFound, service)}
	}
	if errs := BindingConflicts(current, service, bindings); len(errs) > 0 {
		return nil, errs
	}
	out, _, err := AddBindings(current, service, bindings)
	if err != nil {
		return nil, []error{err}
	}
	return out, nil
}

// ServiceExists reports whether the top-level services map contains service.
func ServiceExists(content []byte, service string) bool {
	_, _, _, err := findService(content, service)
	return err == nil
}

// BindingConflicts returns host-port conflicts with this service or any other
// service. Matching is by HostIP, HostPort and Protocol; different container
// ports on the same host port are still a conflict.
func BindingConflicts(content []byte, service string, bindings []model.PortBinding) []error {
	services, err := Services(content)
	if err != nil {
		return []error{err}
	}
	var errs []error
	seen := map[string]model.PortBinding{}
	for _, b := range bindings {
		b = normalizeBinding(b)
		key := hostKey(b)
		if prev, ok := seen[key]; ok && !BindingEqual(prev, b) {
			errs = append(errs, fmt.Errorf("待恢复端口冲突: %s 与 %s", bindingKey(prev), bindingKey(b)))
		}
		seen[key] = b
	}
	for _, svc := range services {
		decls, derr := ReadDecls(content, svc)
		if derr != nil {
			if svc == service && len(bindings) > 0 {
				return append(errs, fmt.Errorf("无法解析服务 %s 的端口: %w", svc, derr))
			}
			continue
		}
		for _, d := range decls {
			for _, b := range bindings {
				b = normalizeBinding(b)
				if d.Binding.Protocol != "tcp" || b.Protocol != "tcp" {
					continue
				}
				if !ipOverlap(normalizeIP(d.Binding.HostIP), normalizeIP(b.HostIP)) || d.Binding.HostPort != b.HostPort {
					continue
				}
				if svc != service {
					errs = append(errs, fmt.Errorf("宿主机端口 %s:%d/%s 已被服务 %s 占用", hostLabelIP(b.HostIP), b.HostPort, b.Protocol, svc))
				} else if !BindingEqual(d.Binding, b) {
					errs = append(errs, fmt.Errorf("服务 %s 已使用宿主机端口 %s:%d 映射到不同容器端口", svc, hostLabelIP(b.HostIP), b.HostPort))
				}
			}
		}
	}
	return errs
}

// Services lists top-level service names.
func Services(content []byte) ([]string, error) {
	var root yaml.Node
	if err := yaml.Unmarshal(content, &root); err != nil {
		return nil, err
	}
	node := &root
	if node.Kind == yaml.DocumentNode && len(node.Content) > 0 {
		node = node.Content[0]
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%w: 顶层不是映射", ErrUnsupported)
	}
	servicesNode := mappingValue(node, "services")
	if servicesNode == nil || servicesNode.Kind == yaml.AliasNode || servicesNode.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%w: 缺少 services 映射", ErrUnsupported)
	}
	var out []string
	for i := 0; i+1 < len(servicesNode.Content); i += 2 {
		out = append(out, servicesNode.Content[i].Value)
	}
	return out, nil
}

func normalizeBinding(b model.PortBinding) model.PortBinding {
	if b.Protocol == "" {
		b.Protocol = "tcp"
	}
	b.Protocol = strings.ToLower(b.Protocol)
	b.HostIP = normalizeIP(b.HostIP)
	return b
}

// ipOverlap treats an unspecified/wildcard address as conflicting with every
// specific address on the same port.
func ipOverlap(a, b string) bool {
	if a == "" || b == "" {
		return true
	}
	return a == b
}

func hostKey(b model.PortBinding) string {
	return fmt.Sprintf("%s|%d|%s", normalizeIP(b.HostIP), b.HostPort, b.Protocol)
}

func hostLabelIP(ip string) string {
	if ip == "" {
		return "0.0.0.0"
	}
	return ip
}
