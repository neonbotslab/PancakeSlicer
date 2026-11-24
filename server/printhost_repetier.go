package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
)

const repetierSoftware = "Repetier-Server"

var repetierSlug = regexp.MustCompile(`^[A-Za-z0-9._~-]{1,200}$`)

func validateRepetierSlug(slug string) error {
	if slug == "" {
		return errors.New("Enter the Repetier Server printer slug from printer/list data[].slug.")
	}
	if !repetierSlug.MatchString(slug) || slug == "." || slug == ".." {
		return errors.New("Use 1–200 ASCII letters, digits, dots, underscores, hyphens, or tildes. A printer slug cannot be a single dot or two dots.")
	}
	return nil
}

func repetierProbe(ctx context.Context, h *hostSession, base *url.URL) error {
	var info struct {
		Software string `json:"software"`
	}
	if _, err := h.call(ctx, "GET", base.JoinPath("printer/info").String(), nil, &info); err != nil {
		return err
	}
	if info.Software != repetierSoftware {
		return fmt.Errorf("The host identified itself as %q instead of %s.", info.Software, repetierSoftware)
	}
	return nil
}

func testRepetier(ctx context.Context, p *PrinterConfig, secret string) PrinterTest {
	h, base, err := httpSession(ctx, p, secret)
	if err == nil {
		err = repetierProbe(ctx, h, base)
	}
	if err != nil {
		return hostFailure(err)
	}
	var list struct {
		Data []struct {
			Name string `json:"name"`
			Slug string `json:"slug"`
		} `json:"data"`
		Error string `json:"error"`
	}
	if _, err := h.call(ctx, "GET", base.JoinPath("printer/list").String(), nil, &list); err != nil {
		return hostFailure(err)
	}
	if list.Error != "" {
		return hostFailure(errors.New(list.Error))
	}
	for _, printer := range list.Data {
		if printer.Slug != p.Slug {
			continue
		}
		name := cmpOr(printer.Name, p.Slug)
		return PrinterTest{OK: true, Stage: "ok", Message: "Connected to Repetier Server. " + name + " uses slug " + p.Slug + "."}
	}
	return hostFailure(fmt.Errorf("Repetier Server printer/list has no data[].slug matching %q.", p.Slug))
}

func statusRepetier(ctx context.Context, h *hostSession, base *url.URL) (PrinterStatus, error) {
	if err := repetierProbe(ctx, h, base); err != nil {
		return PrinterStatus{}, err
	}
	return PrinterStatus{Online: true, State: "unknown", Detail: "Live printer status unavailable"}, nil
}

func repetierUpload(ctx context.Context, base *url.URL, u *upload, body io.Reader) (*http.Request, error) {
	if err := validateRepetierSlug(u.printer.Slug); err != nil {
		return nil, err
	}
	kind := "model"
	fields := [][2]string{{"a", "upload"}}
	if u.start {
		kind = "job"
		fields = [][2]string{{"name", u.filename}, {"autostart", "true"}, {"a", "upload"}}
	}
	target := base.JoinPath("printer", kind, u.printer.Slug).String()
	return multipartUpload(ctx, target, "filename", u.filename, u.size, body, fields)
}
