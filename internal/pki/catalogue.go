package pki

import (
	"crypto"
	"crypto/x509"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"kubemono/internal/constants"
	"kubemono/internal/util/fsutil"
)

// CertID 是叶子证书的稳定标识。
type CertID string

// 叶子证书 ID。
const (
	CertAPIServer              CertID = "apiserver"
	CertAPIServerKubeletClient CertID = "apiserver-kubelet-client"
	CertAPIServerEtcdClient    CertID = "apiserver-etcd-client"
	CertFrontProxyClient       CertID = "front-proxy-client"
	CertControllerManager      CertID = "controller-manager"
	CertScheduler              CertID = "scheduler"
	CertAdmin                  CertID = "admin"
	CertSuperAdmin             CertID = "super-admin"
	CertKubeletClient          CertID = "kubelet-client"
	CertEtcdServer             CertID = "etcd-server"
	CertEtcdPeer               CertID = "etcd-peer"
	CertEtcdKubemonoClient     CertID = "etcd-kubemono-client"
)

// CAID 是证书颁发机构的稳定标识。
type CAID string

const (
	CAKubernetes CAID = "kubernetes-ca"
	CAFrontProxy CAID = "front-proxy-ca"
	CAEtcd       CAID = "etcd-ca"
)

// Store 选择证书所在的 PKI 目录。
type Store string

// PKI 目录。
const (
	StorePKI     Store = "pki"
	StoreEtcdPKI Store = "pki/etcd"
)

// Placement 描述哪些节点需要一张证书。
type Placement uint8

// Placement 取值。
const (
	OnNode              Placement = iota // master 或 worker / master or worker
	OnMaster                             // 仅 master / masters only
	OnMixedMaster                        // 同时是 master 和 worker / master that is also a worker
	OnStackedEtcdMaster                  // 本机运行 stacked etcd / host runs stacked etcd
)

// Target 是判断 Placement 的节点属性。
type Target struct {
	IsMaster bool
	IsWorker bool
	RunsEtcd bool
}

// Subject 描述证书的 CN/O。
// FromNodeName 为 true 时 CN = CommonName + NodeName（例如 system:node: + 节点名）；
// CommonName 为空时 CN 就是节点名。
type Subject struct {
	CommonName    string
	FromNodeName  bool
	Organizations []string
}

// For 返回节点对应的 CN 与 O。
func (s Subject) For(nodeName string) (string, []string) {
	cn := s.CommonName
	if s.FromNodeName {
		cn += nodeName
	}
	return cn, append([]string(nil), s.Organizations...)
}

// ShipSpec 描述一个证书文件是否进入 master join 证书包。
// Required 进入 requiredFiles；Optional 进入 optionalFiles（按存在性打包）；
// 两者都为 false 时只保留为 known 文件（兼容尚未包含该文件的证书包）。
type ShipSpec struct {
	Required bool
	Optional bool
}

// Leaf 是一张叶子证书的唯一声明。
type Leaf struct {
	ID             CertID
	CertFile       string
	KeyFile        string
	Store          Store
	CA             CAID
	Usages         []x509.ExtKeyUsage
	Subject        Subject
	Placement      Placement
	Restart        []string
	KubeconfigFile string
	Ship           *ShipSpec
}

// Kubeconfig 表示该证书由 kubeconfig phase 生成。
func (l Leaf) Kubeconfig() bool { return l.KubeconfigFile != "" }

// Applies 报告该证书是否适用于目标节点。
func (l Leaf) Applies(t Target) bool {
	switch l.Placement {
	case OnNode:
		return t.IsMaster || t.IsWorker
	case OnMaster:
		return t.IsMaster
	case OnMixedMaster:
		return t.IsMaster && t.IsWorker
	case OnStackedEtcdMaster:
		return t.RunsEtcd
	default:
		return false
	}
}

// Path 返回文件在 PKI 目录中的路径。
func (s Store) Path(paths constants.Paths, name string) string {
	if s == StoreEtcdPKI {
		return paths.EtcdCertPath(name)
	}
	return paths.CertPath(name)
}

// FileKey 返回证书包中的规范键（相对 /etc/kubernetes）。
func (s Store) FileKey(name string) string {
	return string(s) + "/" + name
}

// Paths 返回叶子证书/私钥路径。
func (l Leaf) Paths(paths constants.Paths) (cert, key string) {
	return l.Store.Path(paths, l.CertFile), l.Store.Path(paths, l.KeyFile)
}

// SubjectFor 返回节点对应的 CN/O。
func (l Leaf) SubjectFor(nodeName string) (string, []string) { return l.Subject.For(nodeName) }

// CertConfig 返回该叶子证书的基础 CertConfig（SAN 由调用方补充）。
func (l Leaf) CertConfig(nodeName string, validity time.Duration) CertConfig {
	cn, orgs := l.SubjectFor(nodeName)
	return CertConfig{
		CommonName:    cn,
		Organizations: orgs,
		Usages:        append([]x509.ExtKeyUsage(nil), l.Usages...),
		Validity:      validity,
	}
}

// Authority 是一个证书颁发机构的唯一声明。
type Authority struct {
	ID       CAID
	CertFile string
	KeyFile  string
	Store    Store
	Ship     *ShipSpec
}

// Paths 返回 CA 证书/私钥路径。
func (a Authority) Paths(paths constants.Paths) (cert, key string) {
	return a.Store.Path(paths, a.CertFile), a.Store.Path(paths, a.KeyFile)
}

// CN 是创建 CA 时使用的名称（也是 Report 中的 key）。
func (a Authority) CN() string { return string(a.ID) }

// Catalogue 是证书身份的唯一表。
type Catalogue struct {
	Authorities []Authority
	Leaves      []Leaf
}

// 文件权限：证书 0644、私钥 0600。
const (
	CertFileMode fs.FileMode = 0o644
	KeyFileMode  fs.FileMode = 0o600
)

// DefaultCatalogue 是 kubemono 的证书身份表。
var DefaultCatalogue = Catalogue{
	Authorities: []Authority{
		{ID: CAKubernetes, CertFile: constants.CACertName, KeyFile: constants.CAKeyName, Store: StorePKI, Ship: &ShipSpec{Required: true}},
		{ID: CAFrontProxy, CertFile: constants.FrontProxyCACertName, KeyFile: constants.FrontProxyCAKeyName, Store: StorePKI, Ship: &ShipSpec{Required: true}},
		{ID: CAEtcd, CertFile: constants.CACertName, KeyFile: constants.CAKeyName, Store: StoreEtcdPKI, Ship: &ShipSpec{}},
	},
	Leaves: []Leaf{
		{ID: CertAPIServer, CertFile: constants.APIServerCertName, KeyFile: constants.APIServerKeyName, Store: StorePKI, CA: CAKubernetes, Usages: UsageServer, Subject: Subject{CommonName: constants.APIServerCertCN}, Placement: OnNode, Restart: []string{constants.ComponentAPIServer}},
		{ID: CertAPIServerKubeletClient, CertFile: constants.APIServerKubeletClientCertName, KeyFile: constants.APIServerKubeletClientKeyName, Store: StorePKI, CA: CAKubernetes, Usages: UsageClient, Subject: Subject{CommonName: constants.UserAPIServerKubeletClient, Organizations: []string{"system:kubelet-api-admin"}}, Placement: OnNode, Restart: []string{constants.ComponentAPIServer}},
		{ID: CertAPIServerEtcdClient, CertFile: constants.APIServerEtcdClientCertName, KeyFile: constants.APIServerEtcdClientKeyName, Store: StorePKI, CA: CAEtcd, Usages: UsageClient, Subject: Subject{CommonName: "kube-apiserver-etcd-client"}, Placement: OnStackedEtcdMaster, Restart: []string{constants.ComponentAPIServer}, Ship: &ShipSpec{}},
		{ID: CertFrontProxyClient, CertFile: constants.FrontProxyClientCertName, KeyFile: constants.FrontProxyClientKeyName, Store: StorePKI, CA: CAFrontProxy, Usages: UsageClient, Subject: Subject{CommonName: "front-proxy-client"}, Placement: OnNode, Restart: []string{constants.ComponentAPIServer}},
		{ID: CertControllerManager, CertFile: constants.ControllerManagerClientCertName, KeyFile: constants.ControllerManagerClientKeyName, Store: StorePKI, CA: CAKubernetes, Usages: UsageClient, Subject: Subject{CommonName: constants.UserControllerManager}, Placement: OnMaster, Restart: []string{constants.ComponentControllerManager}, KubeconfigFile: constants.ControllerManagerKubeconfigName},
		{ID: CertScheduler, CertFile: constants.SchedulerClientCertName, KeyFile: constants.SchedulerClientKeyName, Store: StorePKI, CA: CAKubernetes, Usages: UsageClient, Subject: Subject{CommonName: constants.UserScheduler}, Placement: OnMaster, Restart: []string{constants.ComponentScheduler}, KubeconfigFile: constants.SchedulerKubeconfigName},
		{ID: CertAdmin, CertFile: constants.AdminClientCertName, KeyFile: constants.AdminClientKeyName, Store: StorePKI, CA: CAKubernetes, Usages: UsageClient, Subject: Subject{CommonName: constants.UserAdmin, Organizations: []string{constants.GroupClusterAdmins}}, Placement: OnMaster, KubeconfigFile: constants.AdminKubeconfigName},
		{ID: CertSuperAdmin, CertFile: constants.SuperAdminClientCertName, KeyFile: constants.SuperAdminClientKeyName, Store: StorePKI, CA: CAKubernetes, Usages: UsageClient, Subject: Subject{CommonName: constants.UserSuperAdmin, Organizations: []string{constants.GroupSystemMasters}}, Placement: OnMaster, KubeconfigFile: constants.SuperAdminKubeconfigName},
		{ID: CertKubeletClient, CertFile: constants.KubeletClientCertName, KeyFile: constants.KubeletClientKeyName, Store: StorePKI, CA: CAKubernetes, Usages: UsageClient, Subject: Subject{CommonName: "system:node:", FromNodeName: true, Organizations: []string{constants.GroupSystemNodes}}, Placement: OnMixedMaster, Restart: []string{constants.ComponentKubelet}, KubeconfigFile: constants.KubeletKubeconfigName},
		{ID: CertEtcdServer, CertFile: constants.EtcdServerCertName, KeyFile: constants.EtcdServerKeyName, Store: StoreEtcdPKI, CA: CAEtcd, Usages: UsageServerClient, Subject: Subject{FromNodeName: true}, Placement: OnStackedEtcdMaster, Restart: []string{constants.ComponentEtcd}},
		{ID: CertEtcdPeer, CertFile: constants.EtcdPeerCertName, KeyFile: constants.EtcdPeerKeyName, Store: StoreEtcdPKI, CA: CAEtcd, Usages: UsageServerClient, Subject: Subject{FromNodeName: true}, Placement: OnStackedEtcdMaster, Restart: []string{constants.ComponentEtcd}},
		{ID: CertEtcdKubemonoClient, CertFile: constants.EtcdKubemonoClientCertName, KeyFile: constants.EtcdKubemonoClientKeyName, Store: StoreEtcdPKI, CA: CAEtcd, Usages: UsageClient, Subject: Subject{CommonName: constants.UserEtcdClient}, Placement: OnStackedEtcdMaster, Ship: &ShipSpec{Optional: true}},
	},
}

// FindLeaf 按 ID、证书文件名或去掉 .crt 的名字查找叶子证书。
func (c Catalogue) FindLeaf(name string) (Leaf, bool) {
	for _, l := range c.Leaves {
		if string(l.ID) == name || l.CertFile == name || strings.TrimSuffix(l.CertFile, ".crt") == name {
			return l, true
		}
	}
	return Leaf{}, false
}

// Authority 按 ID 查找 CA。
func (c Catalogue) Authority(id CAID) (Authority, bool) {
	for _, a := range c.Authorities {
		if a.ID == id {
			return a, true
		}
	}
	return Authority{}, false
}

// MustAuthority 按 ID 查找 CA；不存在时 panic。
func (c Catalogue) MustAuthority(id CAID) Authority {
	a, ok := c.Authority(id)
	if !ok {
		panic("pki: catalogue 缺少 CA " + string(id))
	}
	return a
}

// AllLeaves 返回全部叶子证书（用于续期扫描）。
func (c Catalogue) AllLeaves() []Leaf { return append([]Leaf(nil), c.Leaves...) }

// CertsLeaves 返回 certs phase 负责、且适用于目标节点的叶子证书。
func (c Catalogue) CertsLeaves(t Target) []Leaf {
	var out []Leaf
	for _, l := range c.Leaves {
		if l.Kubeconfig() || !l.Applies(t) {
			continue
		}
		out = append(out, l)
	}
	return out
}

// KubeconfigLeaves 返回 kubeconfig phase 负责的叶子证书。
func (c Catalogue) KubeconfigLeaves() []Leaf {
	var out []Leaf
	for _, l := range c.Leaves {
		if l.Kubeconfig() {
			out = append(out, l)
		}
	}
	return out
}

// ShippedFile 是进入 master join 证书包的一个文件。
type ShippedFile struct {
	Key      string
	Mode     fs.FileMode
	Required bool
	Optional bool
	Store    Store
	Name     string
}

// ShippedFiles 返回 join 证书包中的证书文件集合（不含 SA/encryption）。
func (c Catalogue) ShippedFiles() []ShippedFile {
	var out []ShippedFile
	add := func(ship *ShipSpec, store Store, certFile, keyFile string) {
		if ship == nil {
			return
		}
		out = append(out,
			ShippedFile{Key: store.FileKey(certFile), Mode: CertFileMode, Required: ship.Required, Optional: ship.Optional, Store: store, Name: certFile},
			ShippedFile{Key: store.FileKey(keyFile), Mode: KeyFileMode, Required: ship.Required, Optional: ship.Optional, Store: store, Name: keyFile},
		)
	}
	for _, a := range c.Authorities {
		add(a.Ship, a.Store, a.CertFile, a.KeyFile)
	}
	for _, l := range c.Leaves {
		add(l.Ship, l.Store, l.CertFile, l.KeyFile)
	}
	return out
}

// WriteCertAndKey 原子地写入一张证书及其私钥。
// 证书 0644、私钥 0600；两者都先写临时文件再 rename。
//
// 先在内存里编码完两段 PEM 再落盘：私钥编码失败时不会留下"新证书 + 旧私钥"
// 的半对状态；cert 为 nil 时直接拒绝，避免写出 0 字节的 .crt。
func WriteCertAndKey(certPath, keyPath string, cert *x509.Certificate, key crypto.Signer) error {
	certPEM := EncodeCertPEM(cert)
	if len(certPEM) == 0 {
		return fmt.Errorf("证书为空，拒绝写入 %s", certPath)
	}
	keyPEM, err := EncodeKeyPEM(key)
	if err != nil {
		return err
	}
	if err := ensurePrivateDir(certPath); err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(certPath, certPEM, CertFileMode); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(keyPath, keyPEM, KeyFileMode)
}
