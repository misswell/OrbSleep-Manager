package proxy

import (
	"reflect"
	"testing"
)

func TestRemoveDoesNotCorruptSnapshot(t *testing.T) {
	original := Config{Services: []ServiceConfig{
		{Name: "a", Ports: []string{"9000:80"}},
		{Name: "b", Ports: []string{"9001:80"}},
		{Name: "c", Ports: []string{"9002:80"}},
	}}
	work := original.Clone()
	work.Remove("b")
	work.Services[0].Ports[0] = "9999:80"

	if len(original.Services) != 3 {
		t.Fatalf("original services mutated: %+v", original.Services)
	}
	if original.Services[0].Ports[0] != "9000:80" || original.Services[1].Ports[0] != "9001:80" {
		t.Fatalf("original ports mutated: %+v", original.Services)
	}
	if len(work.Services) != 2 || work.Services[0].Ports[0] != "9999:80" {
		t.Fatalf("working copy wrong: %+v", work.Services)
	}
}

func TestUpsertDoesNotSharePortsSlice(t *testing.T) {
	cfg := Config{}
	ports := []string{"9000:80"}
	cfg.Upsert(ServiceConfig{Name: "a", Ports: ports})
	ports[0] = "9999:80"
	if cfg.Services[0].Ports[0] != "9000:80" {
		t.Fatalf("Upsert kept a shared slice: %+v", cfg.Services[0])
	}
	replacement := ServiceConfig{Name: "a", Ports: []string{"9001:80"}}
	cfg.Upsert(replacement)
	if cfg.Services[0].Ports[0] != "9001:80" {
		t.Fatalf("replacement failed: %+v", cfg.Services[0])
	}
	replacement.Ports[0] = "7777:80"
	if cfg.Services[0].Ports[0] != "9001:80" {
		t.Fatalf("replacement shared slice: %+v", cfg.Services[0])
	}
}

func TestServiceReturnsCopy(t *testing.T) {
	cfg := Config{Services: []ServiceConfig{{Name: "a", Ports: []string{"9000:80"}}}}
	svc, ok := cfg.Service("a")
	if !ok {
		t.Fatal("missing")
	}
	svc.Ports[0] = "1234:80"
	if cfg.Services[0].Ports[0] != "9000:80" {
		t.Fatalf("Service leaked a slice: %+v", cfg.Services[0])
	}
}

func TestMarshalDoesNotMutateOrder(t *testing.T) {
	cfg := Config{Services: []ServiceConfig{{Name: "b"}, {Name: "a"}}}
	_ = cfg.Marshal()
	if cfg.Services[0].Name != "b" || cfg.Services[1].Name != "a" {
		t.Fatalf("Marshal mutated order: %+v", cfg.Services)
	}
	if !reflect.DeepEqual(cfg.Clone().Services, cfg.Services) {
		t.Fatal("clone mismatch")
	}
}
