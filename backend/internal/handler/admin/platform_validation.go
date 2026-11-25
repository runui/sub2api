package admin

import (
	"fmt"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin/binding"
	"github.com/go-playground/validator/v10"
)

// 分组平台使用平台清单；合成路由复用服务层的端点和目标平台规则。
func init() {
	engine, ok := binding.Validator.Engine().(*validator.Validate)
	if !ok {
		panic("platform validation requires a go-playground validator engine")
	}
	if err := engine.RegisterValidation("group_platform", func(fl validator.FieldLevel) bool {
		return domain.IsGroupPlatform(fl.Field().String())
	}); err != nil {
		panic(fmt.Errorf("register group_platform validation: %w", err))
	}
	if err := engine.RegisterValidation("composite_route_endpoint", func(fl validator.FieldLevel) bool {
		return service.IsCompositeRouteEndpoint(fl.Field().String())
	}); err != nil {
		panic(fmt.Errorf("register composite route endpoint validation: %w", err))
	}
	if err := engine.RegisterValidation("composite_route_target", func(fl validator.FieldLevel) bool {
		return service.IsCompositeRouteTarget(fl.Parent().FieldByName("Endpoint").String(), fl.Field().String())
	}); err != nil {
		panic(fmt.Errorf("register composite route target validation: %w", err))
	}
}
