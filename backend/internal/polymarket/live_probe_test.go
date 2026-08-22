package polymarket

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	accountv1 "selfquant/backend/gen/account/v1"
	polymarketv1 "selfquant/backend/gen/polymarket/v1"
)

func TestProductionPrivateReadProbe(t *testing.T) {
	if os.Getenv("POLYMARKET_LIVE_PROBE") != "1" {
		t.Skip("POLYMARKET_LIVE_PROBE is not enabled")
	}
	accountID := int64(6)
	if value := os.Getenv("POLYMARKET_PROBE_ACCOUNT_ID"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 {
			t.Fatalf("invalid POLYMARKET_PROBE_ACCOUNT_ID")
		}
		accountID = parsed
	}
	username := os.Getenv("POLYMARKET_PROBE_USERNAME")
	password := os.Getenv("POLYMARKET_PROBE_PASSWORD")
	if username == "" {
		username = "admin"
	}
	if password == "" {
		password = "admin123"
	}

	accountConnection, err := grpc.NewClient(
		"127.0.0.1:9091", grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer accountConnection.Close()
	accountClient := accountv1.NewAccountServiceClient(accountConnection)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	login, err := accountClient.Login(ctx, &accountv1.LoginRequest{
		Username: username, Password: password,
	})
	if err != nil {
		t.Fatalf("probe login: %v", err)
	}
	request := &accountv1.GetPolymarketCredentialsRequest{
		Token: login.GetToken(), TradingAccountId: accountID,
	}
	before, err := accountClient.GetPolymarketCredentials(ctx, request)
	if err != nil {
		t.Fatalf("load stored credentials: %v", err)
	}
	refreshed, err := accountClient.RefreshPolymarketCredentials(ctx, request)
	if err != nil {
		t.Fatalf("derive and atomically refresh credentials: %v", err)
	}
	if before.GetSignerAddress() != refreshed.GetSignerAddress() ||
		before.GetFunderAddress() != refreshed.GetFunderAddress() ||
		before.GetSignatureType() != refreshed.GetSignatureType() {
		t.Fatal("credential refresh changed signer, funder, or signature type")
	}
	t.Logf(
		"account_id=%d operation=derive-and-compare credential_changed=%t",
		accountID, before.GetApiKey() != refreshed.GetApiKey(),
	)

	polymarketConnection, err := grpc.NewClient(
		"127.0.0.1:9092", grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer polymarketConnection.Close()
	client := polymarketv1.NewPolymarketServiceClient(polymarketConnection)
	summary, err := client.GetAccountSummary(ctx, &polymarketv1.GetAccountSummaryRequest{
		Token: login.GetToken(), TradingAccountId: accountID,
	})
	if err != nil {
		t.Fatalf("account summary probe: %v", err)
	}
	positions, err := client.ListPositions(ctx, &polymarketv1.ListPositionsRequest{
		Token: login.GetToken(), TradingAccountId: accountID,
	})
	if err != nil {
		t.Fatalf("positions probe: %v", err)
	}
	orders, err := client.ListOpenOrders(ctx, &polymarketv1.ListOpenOrdersRequest{
		Token: login.GetToken(), TradingAccountId: accountID,
	})
	if err != nil {
		t.Fatalf("open orders probe: %v", err)
	}
	t.Logf(
		"account_id=%d operation=private-reads summary_stale=%t positions=%d positions_stale=%t orders=%d orders_stale=%t",
		accountID, summary.GetSummary().GetStale(), len(positions.GetItems()),
		positions.GetStale(), len(orders.GetItems()), orders.GetStale(),
	)
}
