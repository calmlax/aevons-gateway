// @title           Aevons Gateway Service API
// @version         1.0.0
// @description     Aevons 自研网关服务，提供统一认证、客户端资源访问控制、Consul 服务发现转发和 Swagger 聚合能力
// @host            localhost:11080
// @BasePath        /
// @schemes         http https

package main

import (
	"os"

	"github.com/calmlax/aevons-gateway/internal/router"

	gatewayconfig "github.com/calmlax/aevons-gateway/internal/config"

	"github.com/calmlax/aevons-framework/core"
	"github.com/calmlax/aevons-framework/xlog"
)

func main() {
	app, err := core.BootstrapWithOptions(core.BootstrapOptions{
		InitDB:      true,
		InitRedis:   true,
		InitGinJSON: true,
	})
	if err != nil {
		xlog.Fatal("failed to bootstrap gateway service: %v", err)
	}
	frameworkCfg, err := app.RawConfig()
	if err != nil {
		xlog.Fatal("failed to read framework config: %v", err)
	}

	configPath := "configs"
	for i, arg := range os.Args {
		if arg == "--config" && i+1 < len(os.Args) {
			configPath = os.Args[i+1]
			break
		}
	}

	env := os.Getenv("APP_ENV")
	if env == "" {
		env = "development"
	}

	settings, err := gatewayconfig.LoadWithConsul(configPath, env, frameworkCfg.Consul)
	if err != nil {
		xlog.Fatal("failed to load gateway config: %v", err)
	}

	engine, err := router.Setup(app, settings)
	if err != nil {
		xlog.Fatal("failed to setup gateway routes: %v", err)
	}

	if err := core.RunGin(app, engine); err != nil {
		xlog.Fatal("failed to run gateway service: %v", err)
	}
}
