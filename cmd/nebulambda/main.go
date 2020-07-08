package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/secretsmanager"
	"github.com/sirupsen/logrus"
	"github.com/slackhq/nebula"
)

// A version string that can be set with
//
//     -ldflags "-X main.Build=SOMEVERSION"
//
// at compile-time.
var Build string

type handler struct {
	dial nebula.Dialer
}

func main() {
	secretName := os.Getenv("NEBULA_SECRET_NAME")
	if secretName == "" {
		log.Fatal("NEBULA_SECRET_NAME not set in env")
	}

	mySession := session.Must(session.NewSession())

	// Create a SecretsManager client from just a session.
	svc := secretsmanager.New(mySession)
	out, err := svc.GetSecretValue(&secretsmanager.GetSecretValueInput{
		SecretId: aws.String(secretName),
	})
	if err != nil {
		log.Fatalf("get secret value: %v", err)
	}

	rawConfig := out.SecretString
	if rawConfig == nil || *rawConfig == "" {
		log.Fatalf("config is empty")
	}

	config := nebula.NewConfig()
	if err := config.LoadString(*rawConfig); err != nil {
		fmt.Printf("failed to load config: %s", err)
		os.Exit(1)
	}

	l := logrus.New()
	l.Out = os.Stdout
	d, err := nebula.Main(config, false, false, Build, l, nil, nil)

	switch v := err.(type) {
	case nebula.ContextualError:
		v.Log(l)
		os.Exit(1)
	case error:
		l.WithError(err).Error("Failed to start")
		os.Exit(1)
	}

	h := &handler{
		dial: d,
	}

	time.Sleep(2 * time.Second)
	lambda.Start(h.handle)
}

type event struct {
	URL string
}

func (h *handler) handle(e event) error {
	if e.URL == "" {
		return fmt.Errorf("invalid event: %#v", e)
	}

	httpClient := http.Client{
		Transport: &http.Transport{
			DialContext: func(_ context.Context, network, addr string) (net.Conn, error) {
				return h.dial(network, addr)
			},
		},
	}

	fmt.Println("path: ", e.URL)
	resp, err := httpClient.Get(e.URL)
	if err != nil {
		return fmt.Errorf("http GET error: %v", err)
	}
	defer resp.Body.Close()

	_, err = io.Copy(os.Stdout, resp.Body)
	if err != nil {
		return fmt.Errorf("body read err: %v", err)
	}

	return nil
}
