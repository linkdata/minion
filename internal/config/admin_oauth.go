package config

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

func (a *AdminConfig) validateOAuth(listenHost string, listenPort uint64) error {
	var cookieHost string
	for _, entry := range []struct {
		name, callbackPath string
		client             *OAuthClient
	}{
		{"github_oauth", "/auth/github/callback", a.GitHubOAuth},
		{"gitlab_oauth", "/auth/gitlab/callback", a.GitLabOAuth},
	} {
		if entry.client == nil {
			continue
		}
		client := entry.client
		if client.ClientID == "" || len(client.ClientID) > 512 || client.ClientSecret == "" || len(client.ClientSecret) > 4096 ||
			strings.ContainsAny(client.ClientID+client.ClientSecret, "\x00\r\n") {
			return fmt.Errorf("admin.%s requires a bounded client_id and client_secret", entry.name)
		}
		redirect, err := url.Parse(client.RedirectURL)
		if err != nil || !validOAuthOrigin(redirect, true) || redirect.Path != entry.callbackPath ||
			strings.ContainsAny(client.RedirectURL, "?#") || redirect.RawPath != "" {
			return fmt.Errorf("admin.%s redirect_url must be an HTTPS or literal loopback IP HTTP %s URL with an ASCII host and no IPv6 zone", entry.name, entry.callbackPath)
		}
		if (redirect.Scheme == "https") != (a.TLSCertFile != "") {
			return fmt.Errorf("admin.%s redirect_url scheme must match admin TLS", entry.name)
		}
		if redirect.Scheme == "http" {
			redirectPort := uint64(80)
			if redirect.Port() != "" {
				redirectPort, _ = strconv.ParseUint(redirect.Port(), 10, 16) // validOAuthOrigin checked the port.
			}
			if listenPort != redirectPort {
				return fmt.Errorf("admin.%s redirect_url HTTP port must match admin.listen", entry.name)
			}
			callbackIP, _ := netip.ParseAddr(redirect.Hostname())
			listenIP, _ := netip.ParseAddr(listenHost)
			if !callbackIP.IsValid() || callbackIP.Unmap() != listenIP.Unmap() {
				return fmt.Errorf("admin.%s redirect_url HTTP host must match admin.listen", entry.name)
			}
		}
		if cookieHost != "" && !strings.EqualFold(cookieHost, redirect.Hostname()) {
			return errors.New("admin OAuth redirect_url hosts must match for the session cookie")
		}
		cookieHost = redirect.Hostname()
		if entry.name == "github_oauth" {
			if client.BaseURL != "" {
				return errors.New("admin.github_oauth base_url is fixed to https://github.com")
			}
			continue
		}
		if client.BaseURL == "" {
			client.BaseURL = "https://gitlab.com"
		}
		base, err := url.Parse(client.BaseURL)
		if err != nil || !validOAuthOrigin(base, false) || base.Path != "" && base.Path != "/" ||
			strings.ContainsAny(client.BaseURL, "?#") || base.RawPath != "" {
			return errors.New("admin.gitlab_oauth base_url must be an HTTPS origin with an ASCII host and no IPv6 zone")
		}
		host := strings.ToLower(base.Host)
		if rawPort := base.Port(); rawPort != "" {
			port, _ := strconv.Atoi(rawPort)
			host = strings.TrimSuffix(host, ":"+rawPort)
			if port != 443 {
				host += ":" + strconv.Itoa(port)
			}
		}
		client.BaseURL = "https://" + host
	}
	return nil
}

func validOAuthOrigin(u *url.URL, allowLoopbackHTTP bool) bool {
	if u == nil || u.Host == "" || strings.HasSuffix(u.Host, ":") || u.User != nil || u.Opaque != "" {
		return false
	}
	host := u.Hostname()
	if host == "" || strings.Contains(host, "%") || strings.IndexFunc(host, func(r rune) bool { return r > 127 }) != -1 {
		return false
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return false
		}
	}
	if u.Scheme == "https" {
		return true
	}
	if u.Scheme != "http" || !allowLoopbackHTTP {
		return false
	}
	addr, err := netip.ParseAddr(host)
	return err == nil && addr.Unmap().IsLoopback()
}
