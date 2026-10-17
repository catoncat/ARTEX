package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Autumn-27/artex/db"
)

// Shodan 资产同步。与 FOFA 同步共用资产入库通道（ip + open_ports，企业
// scope 自动认领），数据源是 Shodan 官方 /shodan/host/search。
// 查询完全被动：数据来自 Shodan 的历史索引，不向目标发送任何流量。
//
// 计费：每次 search 请求扣 1 个 query credit（与返回条数无关）；
// /api-info（余额）免费。余额不足时 API 返回 402。

const (
	shodanConfigKey   = "shodan_config"
	shodanAPIEndpoint = "https://api.shodan.io"
	shodanMaxSize     = 10000
	shodanDefaultSize = 1000
	shodanPageSize    = 100 // Shodan 每页固定 100 条
)

type shodanConfig struct {
	APIKey string `json:"api_key"`
}

func (s *Server) shodanLoadConfig() (shodanConfig, bool) {
	var c shodanConfig
	raw, ok, _ := s.m.pg.GetSetting(shodanConfigKey)
	if ok && raw != "" {
		_ = json.Unmarshal([]byte(raw), &c)
	}
	return c, c.APIKey != ""
}

// ---------- GET/POST /api/sync/shodan/config ----------

func (s *Server) shodanConfigGet(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	c, _ := s.shodanLoadConfig()
	writeJSON(w, 200, map[string]any{
		"key_set": c.APIKey != "",
		"valid":   c.APIKey != "",
	})
}

func (s *Server) shodanConfigSet(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var body shodanConfig
	if err := decode(r, &body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	body.APIKey = strings.TrimSpace(body.APIKey)
	// 空 key = 保留原值（与 FOFA/LLM 配置同语义）。
	old, _ := s.shodanLoadConfig()
	if body.APIKey == "" {
		body.APIKey = old.APIKey
	}
	raw, _ := json.Marshal(body)
	if err := pg.SetSetting(shodanConfigKey, string(raw)); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "valid": body.APIKey != ""})
}

// ---------- Shodan HTTP ----------

type shodanMatch struct {
	IPStr     string   `json:"ip_str"`
	Port      int      `json:"port"`
	Transport string   `json:"transport"`
	Product   string   `json:"product"`
	Version   string   `json:"version"`
	Hostnames []string `json:"hostnames"`
	Domains   []string `json:"domains"`
}

type shodanSearchResp struct {
	Matches []shodanMatch `json:"matches"`
	Total   int           `json:"total"`
}

type shodanQuotaError struct{ msg string }

func (e *shodanQuotaError) Error() string { return e.msg }

func (s *Server) shodanCall(ctx context.Context, path string, query url.Values) ([]byte, error) {
	c, ok := s.shodanLoadConfig()
	if !ok || c.APIKey == "" {
		return nil, fmt.Errorf("Shodan 未配置 API Key")
	}
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	q.Set("key", c.APIKey)
	u := shodanAPIEndpoint + path + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 Shodan 失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode == 401 {
		return nil, fmt.Errorf("Shodan API Key 无效 (401)")
	}
	if resp.StatusCode == 402 {
		return nil, &shodanQuotaError{msg: "Shodan 查询额度不足 (402)，请充值或等待重置"}
	}
	if resp.StatusCode == 403 {
		return nil, fmt.Errorf("Shodan 访问被拒绝 (403)，可能是查询语句违规")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Shodan 错误 (HTTP %d): %s", resp.StatusCode, truncateStr(string(body), 200))
	}
	return body, nil
}

// shodanSearchPage 拉一页（100 条），返回解析后的结果与命中总数。
func (s *Server) shodanSearchPage(ctx context.Context, query string, page int) (*shodanSearchResp, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("page", fmt.Sprint(page))
	q.Set("minify", "true")
	body, err := s.shodanCall(ctx, "/shodan/host/search", q)
	if err != nil {
		return nil, err
	}
	var sr shodanSearchResp
	if err := json.Unmarshal(body, &sr); err != nil {
		return nil, fmt.Errorf("解析 Shodan 响应失败: %s", truncateStr(string(body), 200))
	}
	return &sr, nil
}

// ---------- GET /api/sync/shodan/quota ----------（api-info 免费，不扣额度）

func (s *Server) shodanQuota(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	c, ok := s.shodanLoadConfig()
	if !ok || c.APIKey == "" {
		writeErr(w, 400, "Shodan 未配置 API Key")
		return
	}
	q := url.Values{}
	q.Set("key", c.APIKey)
	u := shodanAPIEndpoint + "/api-info?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		writeErr(w, 502, "请求 Shodan 失败: "+err.Error())
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var info json.RawMessage
	if err := json.Unmarshal(body, &info); err != nil {
		writeErr(w, 502, fmt.Sprintf("解析余额响应失败(HTTP %d)", resp.StatusCode))
		return
	}
	writeJSON(w, 200, map[string]any{"info": info})
}

// ---------- POST /api/sync/shodan/preview ----------（第 1 页：命中总数 + 样例，只扣 1 额度）

type shodanPreviewReq struct {
	Query string `json:"query"`
}

func (s *Server) shodanPreview(w http.ResponseWriter, r *http.Request) {
	if s.pg(w) == nil {
		return
	}
	var body shodanPreviewReq
	if err := decode(r, &body); err != nil || strings.TrimSpace(body.Query) == "" {
		writeErr(w, 400, "query 不能为空")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	sr, err := s.shodanSearchPage(ctx, body.Query, 1)
	if err != nil {
		shodanWriteErr(w, err)
		return
	}
	sample := make([]map[string]any, 0, len(sr.Matches))
	for _, m := range sr.Matches {
		if len(sample) >= 5 {
			break
		}
		sample = append(sample, map[string]any{
			"ip":    m.IPStr,
			"port":  m.Port,
			"product": m.Product,
			"hostnames": m.Hostnames,
		})
	}
	writeJSON(w, 200, map[string]any{"match_size": sr.Total, "sample": sample})
}

func shodanWriteErr(w http.ResponseWriter, err error) {
	if _, ok := err.(*shodanQuotaError); ok {
		writeErr(w, 429, "Shodan 额度不足，请充值或等待重置: "+err.Error())
		return
	}
	writeErr(w, 502, err.Error())
}

// ---------- POST /api/sync/shodan/import ----------

type shodanImportReq struct {
	Query         string `json:"query"`          // Shodan 查询语句，如 ssl:"example.com"
	Size          int    `json:"size"`           // 返回/入库上限，默认 1000，最大 10000
	CompanyName   string `json:"company_name"`   // 入库归属企业；空 = 从查询语句提取
	CreateCompany bool   `json:"create_company"` // 企业不存在时创建并写入资产范围
}

func (s *Server) shodanImport(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	var body shodanImportReq
	if err := decode(r, &body); err != nil || strings.TrimSpace(body.Query) == "" {
		writeErr(w, 400, "query 不能为空")
		return
	}
	size := body.Size
	if size <= 0 {
		size = shodanDefaultSize
	}
	if size > shodanMaxSize {
		size = shodanMaxSize
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	// 分页拉取：每页 100 条，每页扣 1 额度。
	type ipAgg struct {
		ports   []db.PortService
		domains map[string]bool
	}
	agg := map[string]*ipAgg{}
	total := 0
	fetched := 0
	pages := 0
	for page := 1; fetched < size; page++ {
		sr, err := s.shodanSearchPage(ctx, body.Query, page)
		if err != nil {
			shodanWriteErr(w, err)
			return
		}
		pages++
		if page == 1 {
			total = sr.Total
		}
		if len(sr.Matches) == 0 {
			break
		}
		for _, m := range sr.Matches {
			if fetched >= size {
				break
			}
			if m.IPStr == "" {
				continue
			}
			a := agg[m.IPStr]
			if a == nil {
				a = &ipAgg{domains: map[string]bool{}}
				agg[m.IPStr] = a
			}
			for _, h := range m.Hostnames {
				if hd := hostDomain(h); hd != "" {
					a.domains[hd] = true
				}
			}
			for _, d := range m.Domains {
				if hd := hostDomain(d); hd != "" {
					a.domains[hd] = true
				}
			}
			if m.Port > 0 {
				svc := strings.TrimSpace(m.Product)
				if m.Version != "" {
					svc = strings.TrimSpace(svc + " " + m.Version)
				}
				if svc == "" {
					svc = strings.TrimSpace(m.Transport)
				}
				a.ports = append(a.ports, db.PortService{Port: m.Port, Service: svc})
			}
			fetched++
		}
		if fetched >= sr.Total {
			break
		}
	}

	// 企业 + 范围（镜像 FOFA 同步的做法）
	as := s.m.Assets()
	companyName := strings.TrimSpace(body.CompanyName)
	if companyName == "" {
		companyName = shodanQueryRootDomain(body.Query)
	}
	var companies []string
	var errs []string
	var companyID int64
	if body.CreateCompany && companyName != "" {
		cs := as.Companies()
		var cerr error
		companyID, cerr = cs.UpsertByName(companyName)
		if cerr != nil {
			errs = append(errs, "企业 "+companyName+": "+cerr.Error())
		} else {
			companies = append(companies, companyName)
			if root := shodanQueryRootDomain(body.Query); root != "" {
				if _, _, _, aerr := cs.AddScope(companyID, []string{root}, "shodan:"+companyName); aerr != nil {
					errs = append(errs, "scope "+root+": "+strings.Join(aerr, "; "))
				}
			}
		}
	}

	synced := map[string]int{"ip": 0, "port": 0}
	var importedIPs []string
	for ip, a := range agg {
		domains := make([]string, 0, len(a.domains))
		for d := range a.domains {
			domains = append(domains, d)
		}
		if _, uerr := as.UpsertIP(db.UpsertIPReq{IP: ip, BoundDomains: domains, OpenPorts: a.ports}); uerr != nil {
			errs = append(errs, "ip "+ip+": "+uerr.Error())
			continue
		}
		synced["ip"]++
		synced["port"] += len(a.ports)
		importedIPs = append(importedIPs, ip)
	}
	if companyID > 0 && len(importedIPs) > 0 {
		if serr := as.SetIPCompany(companyID, importedIPs); serr != nil {
			errs = append(errs, "企业归属: "+serr.Error())
		}
	}
	writeJSON(w, 200, map[string]any{
		"synced":    synced,
		"companies": companies,
		"errors":    errs,
		"warnings":  []string{fmt.Sprintf("Shodan 命中 %d 条，拉取 %d 页入库 %d 条；本次消耗 %d 查询额度", total, pages, fetched, pages)},
	})
}

// shodanQueryRootDomain 从 ssl:"example.com" / hostname:example.com 语法提取根域。
func shodanQueryRootDomain(q string) string {
	for _, prefix := range []string{`ssl:"`, `hostname:`, `ssl.cert.subject.cn:"`} {
		if i := strings.Index(q, prefix); i >= 0 {
			rest := q[i+len(prefix):]
			rest = strings.TrimPrefix(rest, `"`)
			end := strings.IndexAny(rest, `" `)
			if end < 0 {
				end = len(rest)
			}
			if d := strings.Trim(rest[:end], `"`); d != "" && strings.Contains(d, ".") {
				return d
			}
		}
	}
	return ""
}
