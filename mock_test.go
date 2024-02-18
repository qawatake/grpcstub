package grpcstub_test

import (
	"context"
	"testing"

	"github.com/k1LoW/grpcstub"
	"github.com/k1LoW/grpcstub/testdata/routeguide"
)

func TestMockServer(t *testing.T) {
	ts := grpcstub.NewMockServer()
	ts.Start()
	conn, err := ts.Conn()
	if err != nil {
		t.Fatal(err)
	}
	client := routeguide.NewRouteGuideClient(conn)
	ctx := context.Background()
	res, err := client.GetFeature(ctx, &routeguide.Point{
		Latitude:  10,
		Longitude: 13,
	})
	if err != nil {
		t.Fatal(err)
	}
	got := res.Name
	if want := "hello"; got != want {
		t.Errorf("got %v\nwant %v", got, want)
	}
}
