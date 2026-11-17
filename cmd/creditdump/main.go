// creditdump 诊断工具：只读打印活动投放的原始条目（不领取、不消耗积分）。
//
// 用途：核实上游 delivery 返回里每日签到用什么字段标识（type/triggerEvent），
// 避免签到过滤逻辑依赖会轮换的 campaignId。只做 GET，无写操作。
//
// 用法：creditdump -auth-dir ./auths
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"time"

	"codearts2api/internal/auth"
	"codearts2api/internal/upstream"
)

func main() {
	authDir := flag.String("auth-dir", "./auths", "auth dir")
	flag.Parse()

	auths, err := auth.LoadDir(*authDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	if len(auths) == 0 {
		log.Fatalf("no accounts in %s", *authDir)
	}
	c := upstream.New(60 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, a := range auths {
		cred := upstream.SignCredential{
			AccessKeyID:     a.AccessKeyID,
			SecretAccessKey: a.SecretAccessKey,
			SecurityToken:   a.CloudDragonTok,
		}
		camps, err := c.FetchCreditCampaigns(ctx, cred)
		if err != nil {
			fmt.Printf("account=%s error: %v\n", a.UserID, err)
			continue
		}
		raw, _ := json.MarshalIndent(camps, "", "  ")
		fmt.Printf("account=%s campaigns=%d\n%s\n", a.UserID, len(camps), raw)
	}
}
