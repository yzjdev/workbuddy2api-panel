//go:build ignore

// 实测 Qoder 动态模型拉取：用真实账号 token 打官方 /algo/api/v2/model/list，
// 打印原始响应以诊断为何动态目录为空。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	qoder "github.com/linguo2625469/workbuddy2api-panel/internal/upstream/qoder"
)

var httpClient = &http.Client{}

func httpNewRequest(method, url string) (*http.Request, error) {
	return http.NewRequest(method, url, nil)
}

func main() {
	raw, err := os.ReadFile("auths/qoder-01a0cd1e-5203-7ad1-a81d-cfd0dfa018a7.json")
	if err != nil {
		fmt.Println("read auth:", err)
		return
	}
	var doc struct {
		Auth struct {
			AccessToken string `json:"accessToken"`
			UserType    string `json:"user_type"`
		} `json:"auth"`
		Account struct {
			UID string `json:"uid"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		fmt.Println("parse:", err)
		return
	}
	acct := qoder.Account{
		UID: doc.Account.UID, AccessToken: doc.Auth.AccessToken, UserType: doc.Auth.UserType,
	}
	c := qoder.NewClient(nil)
	entries, err := c.FetchModels(context.Background(), acct)
	fmt.Println("FetchModels err:", err)
	fmt.Println("entries:", len(entries))
	for _, m := range entries {
		fmt.Println("  -", m.Key, m.DisplayName)
	}
	fmt.Println("---- raw diag ----")
	diagFetch(acct)
}

// diagFetch 直接打一次原始请求，打印状态码与响应体前 2KB。
func diagFetch(acct qoder.Account) {
	c := qoder.NewClient(nil)
	sess, err := c.Cache.Get(qoder.AccountLike{
		UID: acct.UID, AccessToken: acct.AccessToken, UserType: acct.UserType,
	})
	if err != nil {
		fmt.Println("session:", err)
		return
	}
	url := qoder.RealmCN.Gateway + qoder.ModelsPath
	req, err := httpNewRequest("GET", url)
	if err != nil {
		fmt.Println("req:", err)
		return
	}
	for k, v := range sess.Headers("", url, "", false) {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		fmt.Println("http:", err)
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	fmt.Println("status:", resp.StatusCode)
	fmt.Printf("body: %s\n", body)
}
