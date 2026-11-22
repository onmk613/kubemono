package etcd

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// newSimClient 构造一个只连模拟集群的 Client。
func newTestClient(cfg Config, r rpc) *Client {
	cfg.applyDefaults()
	return &Client{cfg: cfg, cli: r}
}

func newSimClient(t *testing.T, sim *simRPC, rep *captureReporter, opts ...func(*Config)) *Client {
	t.Helper()
	if rep == nil {
		rep = &captureReporter{}
	}
	cfg := Config{
		Endpoints: sim.endpoints(),
		Report:    rep,
		Now:       fixedNow,
	}
	for _, o := range opts {
		o(&cfg)
	}
	return newTestClient(cfg, sim)
}

func TestMembersParsesLearnerFlag(t *testing.T) {
	sim := newSim(
		simMember{id: 1, name: "m1", healthy: true, dbSize: 1024},
		simMember{id: 2, healthy: false, learner: true},
	)
	c := newSimClient(t, sim, nil)
	members, err := c.Members(context.Background())
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("got %d members, want 2", len(members))
	}
	byID := map[uint64]Member{}
	for _, m := range members {
		byID[m.ID] = m
	}
	if m := byID[1]; m.Name != "m1" || m.IsLearner || m.Role() != RoleVoting {
		t.Errorf("m1 parsed wrong: %+v", m)
	}
	if m := byID[2]; !m.IsLearner || m.Role() != RoleLearner {
		t.Errorf("learner flag lost: %+v", m)
	}
	if m := byID[2]; m.Name != "" {
		t.Errorf("未上报名字的 learner 应保持空名字，got %q", m.Name)
	}

	// JSON 输出必须可直接解码，且 learner 标记可见。
	data, err := json.Marshal(members)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded []map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	learners := 0
	for _, row := range decoded {
		if row["isLearner"] == true {
			learners++
		}
		if row["peerURLs"] == nil {
			t.Errorf("peerURLs missing in JSON: %s", data)
		}
	}
	if learners != 1 {
		t.Errorf("JSON 里应有一个 learner，got %d：%s", learners, data)
	}
}

func TestHealthReportsMembersAndErrors(t *testing.T) {
	sim := newSim(
		simMember{id: 1, name: "m1", healthy: true, dbSize: 2048},
		simMember{id: 2, name: "m2", healthy: true, learner: true, errors: []string{"etcdserver: no leader"}},
		simMember{id: 3, name: "m3", healthy: true, statusErr: errors.New("connection refused")},
	)
	c := newSimClient(t, sim, nil)
	health, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if len(health) != 3 {
		t.Fatalf("got %d endpoints, want 3", len(health))
	}
	byEndpoint := map[string]MemberHealth{}
	for _, h := range health {
		byEndpoint[h.Endpoint] = h
	}
	if h := byEndpoint["https://10.0.0.1:2379"]; !h.Healthy || h.Member != "m1" || h.Status == nil || h.Status.DBSize != 2048 {
		t.Errorf("m1 health wrong: %+v", h)
	}
	if h := byEndpoint["https://10.0.0.2:2379"]; h.Healthy || !strings.Contains(h.Error, "no leader") || h.Role != RoleLearner || h.Member != "m2" {
		t.Errorf("m2 health wrong: %+v", h)
	}
	if h := byEndpoint["https://10.0.0.3:2379"]; h.Healthy || !strings.Contains(h.Error, "connection refused") || h.MemberID != 0 {
		t.Errorf("m3 health wrong: %+v", h)
	}

	data, err := json.Marshal(health)
	if err != nil {
		t.Fatalf("marshal health: %v", err)
	}
	if !strings.Contains(string(data), `"endpoint"`) || !strings.Contains(string(data), `"healthy"`) {
		t.Errorf("health JSON lost fields: %s", data)
	}
}

func TestAddLearnerThenPromoteSequence(t *testing.T) {
	sim := newSim(simMember{id: 1, name: "m1", healthy: true})
	rep := &captureReporter{}
	c := newSimClient(t, sim, rep)
	ctx := context.Background()
	peer := "https://10.0.0.9:2380"

	id, err := c.AddLearner(ctx, "m9", []string{peer})
	if err != nil {
		t.Fatalf("AddLearner: %v", err)
	}
	if id == 0 {
		t.Fatal("AddLearner returned id 0")
	}
	// 幂等：重复调用不发 RPC，返回同一个 ID。
	again, err := c.AddLearner(ctx, "m9", []string{peer})
	if err != nil {
		t.Fatalf("AddLearner (idempotent): %v", err)
	}
	if again != id {
		t.Fatalf("idempotent AddLearner returned %d, want %d", again, id)
	}
	addCalls := 0
	for _, call := range sim.Calls() {
		if call == "MemberAddAsLearner" {
			addCalls++
		}
	}
	if addCalls != 1 {
		t.Fatalf("MemberAddAsLearner called %d times, want 1 (%v)", addCalls, sim.Calls())
	}

	if err := c.Promote(ctx, id); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	// 幂等：已是投票成员时不发 RPC。
	if err := c.Promote(ctx, id); err != nil {
		t.Fatalf("Promote (idempotent): %v", err)
	}
	if sim.promoteCalls != 1 {
		t.Fatalf("MemberPromote called %d times, want 1", sim.promoteCalls)
	}
	if !rep.containsWarn("偶数") {
		t.Errorf("偶数成员应给出警告，got: %s", rep.all())
	}
}

func TestAddLearnerWarnsOnEvenButDoesNotRefuse(t *testing.T) {
	sim := newSim(
		simMember{id: 1, name: "m1", healthy: true},
		simMember{id: 2, name: "m2", healthy: true},
	)
	rep := &captureReporter{}
	c := newSimClient(t, sim, rep)
	if _, err := c.AddLearner(context.Background(), "m3", []string{"https://10.0.0.3:2380"}); err != nil {
		t.Fatalf("AddLearner on even cluster must warn, not fail: %v", err)
	}
	if !rep.containsWarn("偶数") {
		t.Errorf("even member count must warn: %s", rep.all())
	}
}

func TestAddLearnerTreatsLostResponseAsSuccess(t *testing.T) {
	sim := newSim(simMember{id: 1, name: "m1", healthy: true})
	sim.addErr = errors.New("context deadline exceeded")
	sim.addErrButAdds = true
	c := newSimClient(t, sim, nil)
	id, err := c.AddLearner(context.Background(), "m9", []string{"https://10.0.0.9:2380"})
	if err != nil {
		t.Fatalf("响应丢失但成员已存在时应按成功处理：%v", err)
	}
	if id != 2 {
		t.Fatalf("id = %d, want 2", id)
	}
}

func TestPromoteIdempotentWhenAlreadyVotingAfterError(t *testing.T) {
	sim := newSim(simMember{id: 1, name: "m1", healthy: true}, simMember{id: 2, learner: true, peerURL: "https://10.0.0.2:2380"})
	sim.promoteErr = errors.New("etcdserver: learner not ready")
	sim.promoteLate = true
	c := newSimClient(t, sim, nil)
	if err := c.Promote(context.Background(), 2); err != nil {
		t.Fatalf("promote 报错但成员已 voting 时应按成功处理：%v", err)
	}
}

func TestRemoveMemberIdempotent(t *testing.T) {
	sim := newSim(simMember{id: 1, name: "m1", healthy: true})
	c := newSimClient(t, sim, nil)
	ctx := context.Background()
	if err := c.RemoveMember(ctx, 999); err != nil {
		t.Fatalf("移除不存在的成员应当幂等成功：%v", err)
	}
	if sim.removeCalls != 0 {
		t.Fatalf("不存在的成员不应调用 MemberRemove")
	}
	if err := c.RemoveMember(ctx, 1); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if sim.removeCalls != 1 {
		t.Fatalf("removeCalls = %d, want 1", sim.removeCalls)
	}
	if err := c.RemoveMember(ctx, 1); err != nil {
		t.Fatalf("重复移除应当幂等成功：%v", err)
	}
	if sim.removeCalls != 1 {
		t.Fatalf("重复移除不应再调用 RPC，got %d", sim.removeCalls)
	}
}

func TestCanRemoveMemberQuorumCases(t *testing.T) {
	ctx := context.Background()

	t.Run("单成员拒绝", func(t *testing.T) {
		sim := newSim(simMember{id: 1, name: "m1", healthy: true})
		c := newSimClient(t, sim, nil)
		err := c.CanRemoveMember(ctx, 1)
		if err == nil || !strings.Contains(err.Error(), "最后一个投票成员") {
			t.Fatalf("err = %v, want 拒绝移除最后一个成员", err)
		}
		if err := c.CanRemoveMember(ctx, 1); err == nil {
			t.Fatal("单成员集群上移除成员必须拒绝")
		}
	})

	t.Run("两成员允许但告警", func(t *testing.T) {
		sim := newSim(
			simMember{id: 1, name: "m1", healthy: true},
			simMember{id: 2, name: "m2", healthy: true},
		)
		rep := &captureReporter{}
		c := newSimClient(t, sim, rep)
		if err := c.CanRemoveMember(ctx, 2); err != nil {
			t.Fatalf("两成员移除一个应允许：%v", err)
		}
		if !rep.containsWarn("1 个投票成员") {
			t.Errorf("移除后只剩一个成员必须告警：%s", rep.all())
		}
	})

	t.Run("三成员移除健康成员允许", func(t *testing.T) {
		sim := newSim(
			simMember{id: 1, name: "m1", healthy: true},
			simMember{id: 2, name: "m2", healthy: true},
			simMember{id: 3, name: "m3", healthy: true},
		)
		rep := &captureReporter{}
		c := newSimClient(t, sim, rep)
		if err := c.CanRemoveMember(ctx, 3); err != nil {
			t.Fatalf("三成员移除一个应允许：%v", err)
		}
		if !rep.containsWarn("偶数") {
			t.Errorf("移除后两个成员（偶数）应告警：%s", rep.all())
		}
	})

	t.Run("三成员中一个不健康时拒绝移除健康成员", func(t *testing.T) {
		sim := newSim(
			simMember{id: 1, name: "m1", healthy: true},
			simMember{id: 2, name: "m2", healthy: true},
			simMember{id: 3, name: "m3", healthy: true, statusErr: errors.New("connection refused")},
		)
		c := newSimClient(t, sim, nil)
		err := c.CanRemoveMember(ctx, 2)
		if err == nil {
			t.Fatal("移除健康成员会让剩余健康成员只剩 1 个，必须拒绝")
		}
		if !strings.Contains(err.Error(), "健康") {
			t.Fatalf("错误信息应说明健康成员不足：%v", err)
		}
	})

	t.Run("移除不健康成员允许", func(t *testing.T) {
		sim := newSim(
			simMember{id: 1, name: "m1", healthy: true},
			simMember{id: 2, name: "m2", healthy: true},
			simMember{id: 3, name: "m3", healthy: true, statusErr: errors.New("connection refused")},
		)
		c := newSimClient(t, sim, nil)
		if err := c.CanRemoveMember(ctx, 3); err != nil {
			t.Fatalf("移除不健康成员应允许：%v", err)
		}
	})

	t.Run("移除 learner 总是安全", func(t *testing.T) {
		sim := newSim(
			simMember{id: 1, name: "m1", healthy: true, statusErr: errors.New("down")},
			simMember{id: 2, learner: true, peerURL: "https://10.0.0.2:2380"},
		)
		c := newSimClient(t, sim, nil)
		if err := c.CanRemoveMember(ctx, 2); err != nil {
			t.Fatalf("learner 不参与投票，移除必须允许：%v", err)
		}
	})

	t.Run("集群不可达时拒绝", func(t *testing.T) {
		sim := newSim(simMember{id: 1, name: "m1", healthy: true})
		c := newSimClient(t, sim, nil, func(cfg *Config) {
			cfg.Endpoints = []string{"https://10.9.9.9:2379"}
		})
		if err := c.CanRemoveMember(ctx, 1); err == nil {
			t.Fatal("任何 endpoint 都不可达时必须拒绝成员变更")
		}
	})
}

func TestInitialClusterBuildsNamePeerPairs(t *testing.T) {
	members := []Member{
		{ID: 1, Name: "m1", PeerURLs: []string{"https://10.0.0.1:2380"}},
		{ID: 2, Name: "", PeerURLs: []string{"https://10.0.0.2:2380"}},
		{ID: 3, Name: "", PeerURLs: []string{"https://10.0.0.3:2380"}},
	}
	got, err := InitialCluster(members, "m2", "https://10.0.0.2:2380")
	if err != nil {
		t.Fatalf("InitialCluster: %v", err)
	}
	want := []string{"m1=https://10.0.0.1:2380", "m2=https://10.0.0.2:2380", "unnamed-3=https://10.0.0.3:2380"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("InitialCluster = %v, want %v", got, want)
	}
	if _, err := InitialCluster(members, "m9", "https://10.0.0.9:2380"); err == nil {
		t.Fatal("本机 peer URL 不在成员列表时必须报错")
	}
}

func TestFindMemberMatchesPeerURLWhenNameEmpty(t *testing.T) {
	members := []Member{{ID: 7, PeerURLs: []string{"https://10.0.0.7:2380/"}}}
	m, ok := FindMember(members, "m7", []string{"https://10.0.0.7:2380"})
	if !ok || m.ID != 7 {
		t.Fatalf("FindMember = %+v, %v", m, ok)
	}
	if !SamePeerURL("https://10.0.0.7:2380/", "https://10.0.0.7:2380") {
		t.Fatal("SamePeerURL 应容忍结尾斜杠")
	}
}

func TestStatusFromResponseHealthiness(t *testing.T) {
	st := statusFromResponse("https://10.0.0.1:2379", &clientv3.StatusResponse{
		Header:    &pb.ResponseHeader{MemberId: 1},
		Version:   "3.5.15",
		DbSize:    42,
		Leader:    1,
		RaftIndex: 7,
	})
	if !st.Healthy || st.MemberID != 1 || st.DBSize != 42 || st.RaftIndex != 7 {
		t.Fatalf("status = %+v", st)
	}
	bad := statusFromResponse("https://10.0.0.1:2379", &clientv3.StatusResponse{
		Header: &pb.ResponseHeader{MemberId: 1},
		Errors: []string{"etcdserver: no leader"},
	})
	if bad.Healthy {
		t.Fatal("有 Errors 的成员不应判为健康")
	}
}

func TestPermissionDeniedIsTolerated(t *testing.T) {
	sim := newSim(simMember{id: 1, name: "m1", healthy: true})
	sim.getErr = errors.New("etcdserver: permission denied")
	c := newSimClient(t, sim, nil)
	health, err := c.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health[0].Healthy {
		t.Fatalf("permission denied 的线性一致读仍应视为健康：%+v", health[0])
	}
}

func TestConfigDefaults(t *testing.T) {
	cfg := Config{}
	cfg.applyDefaults()
	if cfg.DialTimeout != DefaultDialTimeout || cfg.RequestTimeout != DefaultRequestTimeout {
		t.Fatalf("timeouts not defaulted: %+v", cfg)
	}
	if cfg.Report == nil || cfg.Now == nil {
		t.Fatal("可选依赖必须被填上默认实现")
	}
	if _, err := New(Config{}); err == nil {
		t.Fatal("没有 endpoint 时必须报错")
	}
}

func TestTLSFilesValidation(t *testing.T) {
	if err := (TLSFiles{}).Validate(); err != nil {
		t.Fatalf("Empty TLS must be valid: %v", err)
	}
	err := (TLSFiles{CAFile: "/tmp/ca.crt"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "certFile") {
		t.Fatalf("半配置的 TLS 必须被拒绝，got %v", err)
	}
	if _, err := tlsConfig(TLSFiles{CertFile: "/nope/c.crt", KeyFile: "/nope/c.key", CAFile: "/nope/ca.crt"}); err == nil {
		t.Fatal("证书文件不存在时必须报错")
	}
}

func TestRequestCtxHonoursDeadline(t *testing.T) {
	c := newTestClient(Config{Endpoints: []string{"x"}, RequestTimeout: time.Minute}, newSim())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	got, cancelGot := c.requestCtx(ctx)
	defer cancelGot()
	deadline, ok := got.Deadline()
	if !ok || time.Until(deadline) > time.Second {
		t.Fatalf("已有 deadline 的 ctx 不应被覆盖：%v", deadline)
	}
}
