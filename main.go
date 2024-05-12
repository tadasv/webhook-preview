package main

import (
	"container/ring"
	"context"
	"crypto/rand"
	"embed"
	"encoding/base64"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"sort"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/spf13/cobra"
)

//go:embed view.html index.html
var content embed.FS

type ServerSettings struct {
	ListenAddress string
	HTTPBaseURL   string
}

type RequestEntry struct {
	Request   *http.Request
	Data      []byte
	Timestamp time.Time
}

func NewCLI() *cobra.Command {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	cobra.EnableCommandSorting = false

	rootCmd := &cobra.Command{
		Use:           "webhook-preview",
		Short:         "webhook previewing service",
		SilenceUsage:  true,
		SilenceErrors: true,
		CompletionOptions: cobra.CompletionOptions{
			DisableDefaultCmd: true,
		},
		Run: func(cmd *cobra.Command, args []string) {
			cmd.Print(cmd.UsageString())
		},
	}

	serveCmd := &cobra.Command{
		Use:   "serve",
		Short: "Start webhook-preview service",
		RunE: func(cmd *cobra.Command, args []string) error {
			listenAddress := cmd.Flags().Lookup("listenAddress").Value.String()
			httpBaseURL := cmd.Flags().Lookup("httpBaseURL").Value.String()
			settings := ServerSettings{
				ListenAddress: listenAddress,
				HTTPBaseURL:   httpBaseURL,
			}
			return runServer(cmd.Context(), settings)
		},
	}
	serveCmd.Flags().String("listenAddress", "127.0.0.1:1234", "Address on which the server should listen for request")
	serveCmd.Flags().String("httpBaseURL", "http://localhost:1234", "Base HTTP URL of the server. This should point to LB or some public DNS by which the server can be accessed")

	rootCmd.AddCommand(serveCmd)

	return rootCmd
}

func generateID(l int) string {
	buf := make([]byte, l)
	rand.Read(buf)
	str := base64.RawURLEncoding.EncodeToString(buf)
	return str
}

func captureRequest(r *http.Request) (*RequestEntry, error) {
	timestamp := time.Now().UTC()
	cloned := r.Clone(context.Background())
	defer r.Body.Close()
	data, err := io.ReadAll(r.Body)

	if err == nil {
		return &RequestEntry{
			Request:   cloned,
			Data:      data,
			Timestamp: timestamp,
		}, nil
	}
	return &RequestEntry{
		Request:   cloned,
		Data:      []byte("Server Error: failed to parse data"),
		Timestamp: timestamp,
	}, nil
}

func runServer(ctx context.Context, settings ServerSettings) error {
	router := chi.NewRouter()

	// Store 100 unique endpoints for 30 minutes. Each endpoint is a circular buffer of 10 requests
	ringbufferSize := 10
	requestCache := expirable.NewLRU[string, *ring.Ring](100, nil, 30*time.Minute)

	makeURL := func(v string) string {
		return fmt.Sprintf("%s%s", settings.HTTPBaseURL, v)
	}

	router.Use(middleware.RequestLogger(&middleware.DefaultLogFormatter{
		Logger: log.Default(),
	}))

	router.Get("/", func(w http.ResponseWriter, r *http.Request) {
		t, err := template.ParseFS(content, "index.html")
		if err != nil {
			log.Default().Printf("%#v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		id := generateID(24)

		w.WriteHeader(http.StatusOK)
		if err := t.Execute(w, map[string]any{
			"ViewURL": makeURL(fmt.Sprintf("/view/%s", id)),
		}); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	})

	router.Get("/view/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")

		t, err := template.ParseFS(content, "view.html")
		if err != nil {
			log.Default().Printf("%#v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}

		templateData := map[string]any{
			"EndpointURL": makeURL(fmt.Sprintf("/endpoint/%s", id)),
			"ViewURL":     makeURL(fmt.Sprintf("/view/%s", id)),
			"Requests":    []*RequestEntry{},
		}

		buffer, ok := requestCache.Get(id)
		if ok {
			requestList := []*RequestEntry{}

			buffer.Do(func(v any) {
				if v == nil {
					return
				}

				entry := v.(*RequestEntry)
				requestList = append(requestList, entry)
				println(string(entry.Data))
			})

			sort.Slice(requestList, func(i, j int) bool {
				return requestList[i].Timestamp.After(requestList[j].Timestamp)
			})

			templateData["Requests"] = requestList
		}

		w.WriteHeader(http.StatusOK)
		if err := t.Execute(w, templateData); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	})

	router.Mount("/endpoint/{id}", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		entry, _ := captureRequest(r)

		buffer, ok := requestCache.Get(id)
		if !ok {
			buffer = ring.New(ringbufferSize)
		}

		buffer.Value = entry
		buffer = buffer.Next()

		requestCache.Add(id, buffer)

		w.WriteHeader(http.StatusOK)
	}))

	return http.ListenAndServe(settings.ListenAddress, router)
}

func main() {
	cobra.CheckErr(NewCLI().ExecuteContext(context.Background()))
}
