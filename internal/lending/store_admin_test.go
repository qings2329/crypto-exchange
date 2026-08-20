package lending

import (
	"testing"
)

func TestListAllLendOrders(t *testing.T) {
	store := NewMemStore()

	// Empty store
	all, err := store.ListAllLendOrders()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("expected 0, got %d", len(all))
	}

	// Create 3 orders across different users
	store.CreateLendOrder(&LendOrder{UserID: 1, PoolID: 1, Status: "active"})
	store.CreateLendOrder(&LendOrder{UserID: 2, PoolID: 1, Status: "active"})
	store.CreateLendOrder(&LendOrder{UserID: 1, PoolID: 2, Status: "withdrawn"})

	all, err = store.ListAllLendOrders()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("expected 3, got %d", len(all))
	}
	// Should be sorted by ID
	if all[0].ID != 1 || all[1].ID != 2 || all[2].ID != 3 {
		t.Fatalf("unexpected order: %v %v %v", all[0].ID, all[1].ID, all[2].ID)
	}
}

func TestListAllBorrowOrders(t *testing.T) {
	store := NewMemStore()

	// Empty store
	all, err := store.ListAllBorrowOrders()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("expected 0, got %d", len(all))
	}

	// Create 2 orders
	store.CreateBorrowOrder(&BorrowOrder{UserID: 1, PoolID: 1, Status: "active"})
	store.CreateBorrowOrder(&BorrowOrder{UserID: 3, PoolID: 1, Status: "repaid"})

	all, err = store.ListAllBorrowOrders()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("expected 2, got %d", len(all))
	}
	if all[0].UserID != 1 || all[1].UserID != 3 {
		t.Fatalf("unexpected users: %d %d", all[0].UserID, all[1].UserID)
	}
}

func TestListAllBorrowOrdersDoesNotFilterByStatus(t *testing.T) {
	store := NewMemStore()
	store.CreateBorrowOrder(&BorrowOrder{UserID: 1, PoolID: 1, Status: "active"})
	store.CreateBorrowOrder(&BorrowOrder{UserID: 2, PoolID: 1, Status: "repaid"})
	store.CreateBorrowOrder(&BorrowOrder{UserID: 3, PoolID: 1, Status: "cancelled"})

	all, _ := store.ListAllBorrowOrders()
	if len(all) != 3 {
		t.Fatalf("expected all 3 regardless of status, got %d", len(all))
	}
}

func TestListPoolsEmptyStatusReturnsAll(t *testing.T) {
	store := NewMemStore()
	store.CreatePool(&LendingPool{Asset: "USDT", Status: PoolActive})
	store.CreatePool(&LendingPool{Asset: "ETH", Status: PoolClosed})

	all, _ := store.ListPools("")
	if len(all) != 2 {
		t.Fatalf("expected 2 pools with empty status filter, got %d", len(all))
	}
}
