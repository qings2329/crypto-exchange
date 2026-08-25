package earn

import (
	"time"

	"github.com/coldlar/crypto-exchange/internal/settlement"
)

// Store 是理财中心 + Launchpool 的持久化抽象。Service 只依赖该接口，不关心底层是内存还是 MySQL。
type Store interface {
	// --- 理财产品 ---
	CreateProduct(p *EarnProduct) error
	GetProduct(id int64) (*EarnProduct, error)
	ListProducts(status ProductStatus) ([]*EarnProduct, error)
	UpdateProduct(p *EarnProduct) error

	// --- 理财申购 ---
	CreateSubscription(s *EarnSubscription) error
	GetSubscription(id int64) (*EarnSubscription, error)
	UpdateSubscription(s *EarnSubscription) error
	DeleteSubscription(id int64) error
	ListSubscriptions(userID int64) ([]*EarnSubscription, error)
	ListAllSubscriptions() ([]*EarnSubscription, error)

	// --- Launchpool 项目 ---
	CreateProject(p *LaunchProject) error
	GetProject(id int64) (*LaunchProject, error)
	ListProjects() ([]*LaunchProject, error)
	AddProjectFunded(id int64, d settlement.AssetAmount) error

	// --- Launchpool 仓位 ---
	UpsertPosition(pos *LaunchPosition) error // 按 (user,project,pool) 幂等创建/更新
	DeletePosition(id int64) error
	GetPosition(id int64) (*LaunchPosition, error)
	FindPosition(userID, projectID int64, poolID string) (*LaunchPosition, error)
	ListPositions(userID int64) ([]*LaunchPosition, error)
	ListAllPositions() ([]*LaunchPosition, error)

	Close() error
}

// nowFunc 可注入时钟（测试用）。
type nowFunc = func() time.Time
