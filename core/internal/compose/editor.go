// Package compose locates and surgically edits Docker Compose port mappings.
//
// The editor deliberately works on the original source lines (guided by
// yaml.v3 node positions) instead of re-marshalling a parsed document. This
// keeps comments, ordering, quoting and environment interpolation intact.
package compose

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"orbsleep/internal/model"
)

// PortDecl is a port declaration located in a Compose file.
type PortDecl struct {
	Binding   model.PortBinding
	StartLine int // 1-based inclusive
	EndLine   int // 1-based inclusive
	Short     bool
	Raw       string
}

// Errors reported to the caller.
var (
	ErrServiceNotFound = errors.New("compose service not found")
	ErrUnsupported     = errors.New("compose 文件结构不受支持")
)

// ReadDecls parses the ports declared for a service in a Compose file.
func ReadDecls(content []byte, service string) ([]PortDecl, error) {
	_, _, svc, err := findService(content, service)
	if err != nil {
		return nil, err
	}
	portsNode := mappingValue(svc, "ports")
	if portsNode == nil {
		return nil, nil
	}
	if portsNode.Kind == yaml.AliasNode {
		return nil, fmt.Errorf("%w: ports 使用 YAML 别名", ErrUnsupported)
	}
	if portsNode.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("%w: ports 不是列表", ErrUnsupported)
	}
	if portsNode.Style&yaml.FlowStyle != 0 {
		return nil, fmt.Errorf("%w: ports 使用流式（单行）写法", ErrUnsupported)
	}
	var out []PortDecl
	for _, item := range portsNode.Content {
		decl, err := declFromNode(item)
		if err != nil {
			return nil, err
		}
		out = append(out, *decl)
	}
	return out, nil
}

func declFromNode(n *yaml.Node) (*PortDecl, error) {
	if n.Anchor != "" {
		return nil, fmt.Errorf("%w: 端口列表项使用锚点", ErrUnsupported)
	}
	switch n.Kind {
	case yaml.ScalarNode:
		b, err := ParseShort(n.Value)
		if err != nil {
			return nil, err
		}
		return &PortDecl{Binding: b, StartLine: n.Line, EndLine: n.Line, Short: true, Raw: n.Value}, nil
	case yaml.MappingNode:
		b, err := parseLong(n)
		if err != nil {
			return nil, err
		}
		return &PortDecl{Binding: b, StartLine: n.Line, EndLine: maxLine(n), Short: false, Raw: n.Value}, nil
	case yaml.AliasNode:
		return nil, fmt.Errorf("%w: 端口列表项使用别名", ErrUnsupported)
	default:
		return nil, fmt.Errorf("%w: 无法识别的端口写法", ErrUnsupported)
	}
}

// RemoveBindings removes the given host bindings from a service's ports list.
// It returns the new file content and the declarations that were removed.
func RemoveBindings(content []byte, service string, remove []model.PortBinding) ([]byte, []PortDecl, error) {
	doc, _, svc, err := findService(content, service)
	if err != nil {
		return nil, nil, err
	}
	portsKey := mappingKeyNode(svc, "ports")
	portsNode := mappingValue(svc, "ports")
	if portsNode == nil {
		return nil, nil, ErrServiceNotFound
	}
	if portsNode.Kind == yaml.AliasNode || portsNode.Style&yaml.FlowStyle != 0 {
		return nil, nil, fmt.Errorf("%w: ports 不是块状列表", ErrUnsupported)
	}
	if portsNode.Kind != yaml.SequenceNode {
		return nil, nil, fmt.Errorf("%w: ports 不是列表", ErrUnsupported)
	}
	if len(remove) == 0 {
		return content, nil, nil
	}
	// Guard: ports key must be alone on its line, otherwise deleting the key
	// would also delete unrelated content.
	if portsKey.Line == portsNode.Line {
		return nil, nil, fmt.Errorf("%w: ports 声明与列表在同一行", ErrUnsupported)
	}

	lines := strings.Split(string(content), "\n")
	var removed []PortDecl
	del := map[int]bool{}
	remaining := len(portsNode.Content)
	for _, item := range portsNode.Content {
		decl, err := declFromNode(item)
		if err != nil {
			return nil, nil, err
		}
		if !matchesAny(decl.Binding, remove) {
			continue
		}
		removed = append(removed, *decl)
		remaining--
		for line := decl.StartLine; line <= decl.EndLine && line-1 < len(lines); line++ {
			del[line-1] = true
		}
	}
	if len(removed) != len(remove) {
		return nil, nil, fmt.Errorf("Compose 文件中的端口声明与容器实际映射不一致（找到 %d，需要移除 %d）；请重新扫描", len(removed), len(remove))
	}
	if remaining == 0 {
		// Remove the now-empty `ports:` key as well.
		del[portsKey.Line-1] = true
	}

	var kept []string
	for i, line := range lines {
		if !del[i] {
			kept = append(kept, line)
		}
	}
	out := []byte(strings.Join(kept, "\n"))
	if err := ValidateRemoval(content, out, service, remove); err != nil {
		return nil, nil, err
	}
	_ = doc
	return out, removed, nil
}

// ValidateRemoval asserts that the new document differs from the old one only
// by the removed port entries.
func ValidateRemoval(before, after []byte, service string, remove []model.PortBinding) error {
	var oldMap, newMap map[string]any
	if err := yaml.Unmarshal(before, &oldMap); err != nil {
		return fmt.Errorf("解析原始 Compose 文件失败: %w", err)
	}
	if err := yaml.Unmarshal(after, &newMap); err != nil {
		return fmt.Errorf("修改后的 Compose 文件不是合法 YAML: %w", err)
	}
	oldSvc := serviceMap(oldMap, service)
	if oldSvc == nil {
		return ErrServiceNotFound
	}
	oldPorts, _ := oldSvc["ports"].([]any)
	var expected []any
	for _, item := range oldPorts {
		b, err := ParseAny(item)
		if err != nil {
			// Unknown syntax: keep it and let the structural comparison fail
			// only if something else changed.
			expected = append(expected, item)
			continue
		}
		if matchesAny(b, remove) {
			continue
		}
		expected = append(expected, item)
	}
	newSvc := serviceMap(newMap, service)
	if newSvc == nil {
		return fmt.Errorf("修改后的 Compose 文件丢失了服务 %q", service)
	}
	newPorts, _ := newSvc["ports"].([]any)
	if len(expected) == 0 {
		delete(oldSvc, "ports")
		delete(newSvc, "ports")
	} else {
		oldSvc["ports"] = expected
		newSvc["ports"] = newPorts
	}
	if !reflect.DeepEqual(oldMap, newMap) {
		return errors.New("修改后的 Compose 配置与预期不一致（除目标端口外还有其他差异），已中止")
	}
	return nil
}

// DiffForRemovals renders a compact unified diff for the removed lines.
func DiffForRemovals(before string, decls []PortDecl, filename string) string {
	if len(decls) == 0 {
		return ""
	}
	lines := strings.Split(before, "\n")
	var ranges [][2]int
	for _, d := range decls {
		if n := len(ranges); n > 0 && d.StartLine <= ranges[n-1][1]+1 {
			ranges[n-1][1] = d.EndLine
			continue
		}
		ranges = append(ranges, [2]int{d.StartLine, d.EndLine})
	}
	const ctx = 3
	display := filepath.Base(filename)
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", display, display)
	deletedSoFar := 0
	for _, r := range ranges {
		start := r[0] - ctx
		if start < 1 {
			start = 1
		}
		end := r[1] + ctx
		if end > len(lines) {
			end = len(lines)
		}
		oldCount := end - start + 1
		newStart := start - deletedSoFar
		if newStart < 1 {
			newStart = 1
		}
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", start, oldCount, newStart, oldCount-(r[1]-r[0]+1))
		for i := start; i <= end; i++ {
			if i >= r[0] && i <= r[1] {
				b.WriteString("-" + lines[i-1] + "\n")
			} else {
				b.WriteString(" " + lines[i-1] + "\n")
			}
		}
		deletedSoFar += r[1] - r[0] + 1
	}
	return b.String()
}

// ParseShort parses the short port syntax: "8080:80", "127.0.0.1:8080:80",
// "[::1]:8080:80", "80", with an optional "/udp" suffix.
func ParseShort(s string) (model.PortBinding, error) {
	raw := strings.TrimSpace(s)
	if raw == "" {
		return model.PortBinding{}, fmt.Errorf("%w: 空端口声明", ErrUnsupported)
	}
	proto := "tcp"
	if i := strings.LastIndex(raw, "/"); i >= 0 {
		proto = strings.ToLower(raw[i+1:])
		raw = raw[:i]
	}
	if proto != "tcp" && proto != "udp" {
		return model.PortBinding{}, fmt.Errorf("%w: 未知协议 %q", ErrUnsupported, proto)
	}
	var hostIP string
	if strings.HasPrefix(raw, "[") {
		end := strings.Index(raw, "]")
		if end < 0 {
			return model.PortBinding{}, fmt.Errorf("%w: 非法 IPv6 端口声明", ErrUnsupported)
		}
		hostIP = raw[1:end]
		raw = strings.TrimPrefix(raw[end+1:], ":")
	}
	parts := strings.Split(raw, ":")
	b := model.PortBinding{Protocol: proto}
	switch len(parts) {
	case 1:
		// Container port only -> Docker picks a random host port.
		cp, err := atoiPort(parts[0])
		if err != nil {
			return b, err
		}
		b.ContainerPort = cp
		return b, nil
	case 2:
		hp, err := atoiPort(parts[0])
		if err != nil {
			return b, err
		}
		cp, err := atoiPort(parts[1])
		if err != nil {
			return b, err
		}
		b.HostIP = normalizeIP(hostIP)
		b.HostPort, b.ContainerPort = hp, cp
	case 3:
		hp, err := atoiPort(parts[1])
		if err != nil {
			return b, err
		}
		cp, err := atoiPort(parts[2])
		if err != nil {
			return b, err
		}
		b.HostIP = normalizeIP(parts[0])
		b.HostPort, b.ContainerPort = hp, cp
	default:
		return b, fmt.Errorf("%w: 无法解析端口声明 %q", ErrUnsupported, s)
	}
	return b, nil
}

// ParseAny parses either the short or the long syntax from a decoded YAML value.
func ParseAny(v any) (model.PortBinding, error) {
	switch x := v.(type) {
	case string:
		return ParseShort(x)
	case int:
		return model.PortBinding{ContainerPort: x, Protocol: "tcp"}, nil
	case map[string]any:
		b := model.PortBinding{Protocol: "tcp"}
		if t, ok := x["target"]; ok {
			b.ContainerPort = anyInt(t)
		}
		if p, ok := x["published"]; ok {
			b.HostPort = anyInt(p)
		}
		if h, ok := x["host_ip"].(string); ok {
			b.HostIP = normalizeIP(h)
		}
		if p, ok := x["protocol"].(string); ok && p != "" {
			b.Protocol = strings.ToLower(p)
		}
		if b.ContainerPort == 0 {
			return b, fmt.Errorf("%w: 长语法缺少 target", ErrUnsupported)
		}
		return b, nil
	}
	return model.PortBinding{}, fmt.Errorf("%w: 无法识别的端口项 %T", ErrUnsupported, v)
}

func parseLong(n *yaml.Node) (model.PortBinding, error) {
	var m map[string]any
	if err := n.Decode(&m); err != nil {
		return model.PortBinding{}, err
	}
	return ParseAny(m)
}

func findService(content []byte, service string) (*yaml.Node, *yaml.Node, *yaml.Node, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(content, &doc); err != nil {
		return nil, nil, nil, fmt.Errorf("解析 Compose YAML 失败: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return nil, nil, nil, fmt.Errorf("%w: 空文档", ErrUnsupported)
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, nil, nil, fmt.Errorf("%w: 顶层不是映射", ErrUnsupported)
	}
	if root.Style&yaml.FlowStyle != 0 {
		return nil, nil, nil, fmt.Errorf("%w: 顶层使用流式写法", ErrUnsupported)
	}
	services := mappingValue(root, "services")
	if services == nil || services.Kind != yaml.MappingNode {
		return &doc, root, nil, ErrServiceNotFound
	}
	svc := mappingValue(services, service)
	if svc == nil {
		return &doc, root, nil, ErrServiceNotFound
	}
	if svc.Kind != yaml.MappingNode {
		return &doc, root, nil, fmt.Errorf("%w: 服务 %q 不是映射", ErrUnsupported, service)
	}
	if mappingKeyNode(svc, "extends") != nil {
		return &doc, root, nil, fmt.Errorf("%w: 服务使用 extends 继承", ErrUnsupported)
	}
	if mappingKeyNode(svc, "<<") != nil {
		return &doc, root, nil, fmt.Errorf("%w: 服务使用 YAML 合并键 <<", ErrUnsupported)
	}
	return &doc, root, svc, nil
}

func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func mappingKeyNode(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i]
		}
	}
	return nil
}

func maxLine(n *yaml.Node) int {
	max := n.Line
	for _, c := range n.Content {
		if l := maxLine(c); l > max {
			max = l
		}
	}
	return max
}

func matchesAny(b model.PortBinding, remove []model.PortBinding) bool {
	for _, r := range remove {
		if BindingEqual(b, r) {
			return true
		}
	}
	return false
}

// BindingEqual compares bindings, treating "" and 0.0.0.0 as equivalent.
func BindingEqual(a, b model.PortBinding) bool {
	pa, pb := a.Protocol, b.Protocol
	if pa == "" {
		pa = "tcp"
	}
	if pb == "" {
		pb = "tcp"
	}
	return pa == pb &&
		normalizeIP(a.HostIP) == normalizeIP(b.HostIP) &&
		a.HostPort == b.HostPort &&
		a.ContainerPort == b.ContainerPort
}

func normalizeIP(ip string) string {
	switch strings.TrimSpace(ip) {
	case "", "0.0.0.0":
		return ""
	default:
		return strings.TrimSpace(ip)
	}
}

func atoiPort(s string) (int, error) {
	if strings.Contains(s, "-") {
		return 0, fmt.Errorf("%w: 不支持端口范围 %q", ErrUnsupported, s)
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 0 || n > 65535 {
		return 0, fmt.Errorf("%w: 非法端口 %q", ErrUnsupported, s)
	}
	return n, nil
}

func anyInt(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int64:
		return int(x)
	case uint64:
		return int(x)
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	}
	return 0
}

func serviceMap(m map[string]any, service string) map[string]any {
	services, _ := m["services"].(map[string]any)
	if services == nil {
		return nil
	}
	svc, _ := services[service].(map[string]any)
	return svc
}

// BackupFile copies file into backupDir, preserving its mode and relative name.
func BackupFile(file, backupDir string) (string, error) {
	info, err := os.Stat(file)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return "", err
	}
	dest := filepath.Join(backupDir, filepath.Base(file))
	if err := copyFile(file, dest, info.Mode()); err != nil {
		return "", err
	}
	return dest, nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	raw, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return WriteAtomic(dst, raw, mode)
}

// RestoreFile atomically restores file from backup.
func RestoreFile(file, backup string) error {
	info, err := os.Stat(backup)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(backup)
	if err != nil {
		return err
	}
	return WriteAtomic(file, raw, info.Mode())
}

// WriteAtomic writes data to a temp file in the same directory and renames it.
func WriteAtomic(file string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(file)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".orbsleep-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(name, mode); err != nil {
		return err
	}
	return os.Rename(name, file)
}

// ValidateEffective compares two `docker compose config --format json` outputs
// and asserts they differ only by the removed port mappings.
func ValidateEffective(before, after map[string]any, service string, remove []model.PortBinding) error {
	oldMap := deepCopyMap(before)
	newMap := deepCopyMap(after)
	oldSvc := serviceMap(oldMap, service)
	newSvc := serviceMap(newMap, service)
	if oldSvc == nil || newSvc == nil {
		return fmt.Errorf("有效配置中找不到服务 %q", service)
	}
	oldPorts, _ := oldSvc["ports"].([]any)
	var expected []any
	matched := 0
	for _, item := range oldPorts {
		p, err := ParseAny(item)
		if err != nil {
			expected = append(expected, item)
			continue
		}
		if matchesAny(p, remove) {
			matched++
			continue
		}
		expected = append(expected, item)
	}
	newPorts, _ := newSvc["ports"].([]any)
	if len(expected) == 0 {
		delete(oldSvc, "ports")
		delete(newSvc, "ports")
	} else {
		oldSvc["ports"] = expected
		newSvc["ports"] = newPorts
	}
	if matched != len(remove) {
		return fmt.Errorf("有效配置中只找到 %d/%d 个待移除端口；Compose 解析结果与容器实际端口不一致", matched, len(remove))
	}
	if !reflect.DeepEqual(oldMap, newMap) {
		return errors.New("修改 Compose 后，除目标端口外还有其他有效配置发生变化，已中止")
	}
	return nil
}

func deepCopyMap(in map[string]any) map[string]any {
	raw, err := json.Marshal(in)
	if err != nil {
		return in
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return in
	}
	return out
}

// AddBindings re-adds host port mappings to a service, preserving the rest of
// the file. It is used when detaching a service: restoring a whole backup would
// also revert ports of other managed services that share the same Compose file.
func AddBindings(content []byte, service string, add []model.PortBinding) ([]byte, []PortDecl, error) {
	_, root, svc, err := findService(content, service)
	if err != nil {
		return nil, nil, err
	}
	if len(add) == 0 {
		return content, nil, nil
	}
	portsKey := mappingKeyNode(svc, "ports")
	portsNode := mappingValue(svc, "ports")
	lines := strings.Split(string(content), "\n")

	// Does the service already declare some/all of the requested mappings?
	existing, err := ReadDecls(content, service)
	if err != nil && portsNode != nil {
		return nil, nil, err
	}
	have := map[string]bool{}
	for _, d := range existing {
		have[bindingKey(d.Binding)] = true
	}
	var todo []model.PortBinding
	for _, b := range add {
		if b.Protocol == "" {
			b.Protocol = "tcp"
		}
		if have[bindingKey(b)] {
			continue
		}
		todo = append(todo, b)
	}
	if len(todo) == 0 {
		return content, nil, nil
	}

	if svc.Style&yaml.FlowStyle != 0 {
		return nil, nil, fmt.Errorf("%w: 服务使用流式写法，无法安全添加端口", ErrUnsupported)
	}
	// Determine indentation from the service key and existing entries.
	serviceKeyIndent := serviceKeyNode(root, service).Column - 1
	itemIndent := serviceKeyIndent + 4
	keyIndent := serviceKeyIndent + 2
	insertAfter := 0

	if portsNode != nil {
		if portsNode.Kind != yaml.SequenceNode || portsNode.Style&yaml.FlowStyle != 0 || portsKey.Line == portsNode.Line {
			return nil, nil, fmt.Errorf("%w: ports 不是块状列表，无法安全添加端口", ErrUnsupported)
		}
		keyIndent = portsKey.Column - 1
		if len(portsNode.Content) > 0 {
			first := portsNode.Content[0]
			// Column points at the scalar/key after "- ", so the dash itself
			// sits two columns earlier.
			itemIndent = first.Column - 1 - 2
			if itemIndent < keyIndent+2 {
				itemIndent = keyIndent + 2
			}
			last := portsNode.Content[len(portsNode.Content)-1]
			insertAfter = maxLine(last)
		} else {
			insertAfter = portsKey.Line
			itemIndent = keyIndent + 2
		}
	} else {
		insertAfter = maxLine(svc)
	}
	if insertAfter <= 0 || insertAfter > len(lines) {
		return nil, nil, fmt.Errorf("%w: 无法定位服务块的插入位置", ErrUnsupported)
	}

	var newLines []string
	if portsNode == nil {
		newLines = append(newLines, strings.Repeat(" ", keyIndent)+"ports:")
	}
	var added []PortDecl
	for _, b := range todo {
		text := quoteShort(b)
		newLines = append(newLines, strings.Repeat(" ", itemIndent)+"- \""+text+"\"")
		added = append(added, PortDecl{Binding: b, StartLine: insertAfter + len(newLines), EndLine: insertAfter + len(newLines), Short: true, Raw: text})
	}
	out := make([]string, 0, len(lines)+len(newLines))
	out = append(out, lines[:insertAfter]...)
	out = append(out, newLines...)
	out = append(out, lines[insertAfter:]...)
	result := []byte(strings.Join(out, "\n"))
	if err := ValidateAddition(content, result, service, todo); err != nil {
		return nil, nil, err
	}
	return result, added, nil
}

// ValidateAddition asserts the new document differs from the old one only by
// the added port entries.
func ValidateAddition(before, after []byte, service string, add []model.PortBinding) error {
	var oldMap, newMap map[string]any
	if err := yaml.Unmarshal(before, &oldMap); err != nil {
		return err
	}
	if err := yaml.Unmarshal(after, &newMap); err != nil {
		return fmt.Errorf("添加端口后的 Compose 文件不是合法 YAML: %w", err)
	}
	oldSvc := serviceMap(oldMap, service)
	newSvc := serviceMap(newMap, service)
	if oldSvc == nil || newSvc == nil {
		return fmt.Errorf("服务 %q 丢失", service)
	}
	oldPorts, _ := oldSvc["ports"].([]any)
	var expected []any
	expected = append(expected, oldPorts...)
	for _, b := range add {
		expected = append(expected, quoteShort(b))
	}
	newPorts, _ := newSvc["ports"].([]any)
	if len(newPorts) != len(expected) {
		return fmt.Errorf("添加端口后的端口数量不符合预期（%d != %d）", len(newPorts), len(expected))
	}
	oldSvc["ports"] = expected
	newSvc["ports"] = newPorts
	if !reflect.DeepEqual(oldMap, newMap) {
		return errors.New("添加端口时意外修改了其他配置，已中止")
	}
	return nil
}

func quoteShort(b model.PortBinding) string {
	proto := b.Protocol
	if proto == "" || proto == "tcp" {
		proto = ""
	} else {
		proto = "/" + proto
	}
	if b.HostIP == "" {
		return fmt.Sprintf("%d:%d%s", b.HostPort, b.ContainerPort, proto)
	}
	return fmt.Sprintf("%s:%d:%d%s", b.HostIP, b.HostPort, b.ContainerPort, proto)
}

func bindingKey(b model.PortBinding) string {
	proto := b.Protocol
	if proto == "" {
		proto = "tcp"
	}
	return fmt.Sprintf("%s|%d|%d|%s", normalizeIP(b.HostIP), b.HostPort, b.ContainerPort, proto)
}

func serviceKeyNode(root *yaml.Node, service string) *yaml.Node {
	services := mappingValue(root, "services")
	if services == nil {
		return root
	}
	return mappingKeyNode(services, service)
}
