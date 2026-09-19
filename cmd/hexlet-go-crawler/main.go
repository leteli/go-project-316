package main

import (
	"code/crawler"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"time"

	"github.com/urfave/cli/v3"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	app := &cli.Command{
		Name:  "hexlet-go-crawler",
		Usage: "analyze a website structure",
		Flags: []cli.Flag{
			&cli.Int64Flag{
				Name:  "depth",
				Value: 10,
				Usage: "crawl depth",
			},
			&cli.Int64Flag{
				Name:  "retries",
				Value: 1,
				Usage: "number of retries for failed requests",
			},
			&cli.StringFlag{
				Name:  "delay",
				Value: "0s",
				Usage: "delay between requests (example: 200ms, 1s)",
			},
			&cli.StringFlag{
				Name:  "timeout",
				Value: "15s",
				Usage: "per-request timeout (example: 200ms, 1s)",
			},
			&cli.Int64Flag{
				Name:  "rps",
				Value: 0,
				Usage: "limit requests per second (overrides delay)",
			},
			&cli.StringFlag{
				Name:  "user-agent",
				Usage: "custom user agent",
			},
			&cli.Int64Flag{
				Name:  "workers",
				Value: 4,
				Usage: "number of concurrent workers",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			ctx, cancel := context.WithTimeout(ctx, 1*time.Minute)
			defer cancel()

			URL := cmd.Args().Get(0)
			if URL == "" {
				return errors.New("URL argument is required")
			}
			timeout := cmd.String("timeout")
			d, err := time.ParseDuration(timeout)
			if err != nil {
				d = 15 * time.Second
			}
			httpClient := &http.Client{
				Timeout: d,
				Transport: &http.Transport{
					DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
						host, port, err := net.SplitHostPort(addr)
						if err != nil {
							return nil, err
						}
						ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
						if err != nil {
							return nil, err
						}
						if len(ips) == 0 {
							return nil, errors.New("no ips detected")
						}
						for _, ip := range ips {
							if ip.IsLoopback() {
								return nil, fmt.Errorf("ip %s is a loopback address (blocked)", ip)
							}
							if ip.IsPrivate() {
								return nil, fmt.Errorf("ip %s is a private network address (blocked)", ip)
							}
							if ip.IsLinkLocalUnicast() {
								return nil, fmt.Errorf("ip %s is a link-local address (blocked)", ip)
							}
						}
						if ips[0].String() == "" {
							return nil, errors.New("ip must be resolved")
						}
						dialer := &net.Dialer{Timeout: 5 * time.Second}
						return dialer.DialContext(ctx, network, net.JoinHostPort(ips[0].String(), port))
					},
				},
			}

			opts := crawler.Options{
				URL:     URL,
				Depth:   cmd.Int64("depth"),
				Retries: cmd.Int64("retries"),
				Delay:   cmd.String("delay"),
				Timeout: timeout,
				// UserAgent   *string
				Concurrency: cmd.Int64("workers"),
				// TODO: check future flags spec
				IndentJSON: " ",
				HTTPClient: httpClient,
			}
			bytes, err := crawler.Analyze(ctx, opts)
			if err != nil {
				return err
			}
			fmt.Println(string(bytes))
			return nil
		},
	}
	return app.Run(context.Background(), os.Args)
}
