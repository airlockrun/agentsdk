package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	airlockv1 "github.com/airlockrun/agentsdk/internal/airlockv1"
	"golang.org/x/term"
)

type credentialsFile struct {
	Sessions map[string]credentialSession `json:"sessions"`
}

type credentialSession struct {
	Email        string `json:"email,omitempty"`
	AccessToken  string `json:"accessToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
}

func cmdLogin(args []string) error {
	var email string
	positional, err := parseFlags(args, func(key, value string) error {
		switch key {
		case "email":
			email = value
		default:
			return fmt.Errorf("unknown flag --%s", key)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("login requires exactly one argument: the Airlock URL")
	}
	baseURL := normalizeBaseURL(positional[0])
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return errors.New("Airlock URL must start with http:// or https://")
	}
	if email == "" {
		fmt.Print("Email: ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return err
		}
		email = strings.TrimSpace(line)
	}
	password := os.Getenv("AIRLOCK_PASSWORD")
	if password == "" {
		if !term.IsTerminal(int(os.Stdin.Fd())) {
			return errors.New("AIRLOCK_PASSWORD is required when stdin is not a terminal")
		}
		fmt.Print("Password: ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Println()
		if err != nil {
			return err
		}
		password = string(b)
	}

	var resp airlockv1.LoginResponse
	if err := doProto(context.Background(), baseURL, httpMethodPost, "/auth/login", "", &airlockv1.LoginRequest{Email: email, Password: password}, &resp); err != nil {
		return fmt.Errorf("login: %w", err)
	}
	creds, err := loadCredentials()
	if err != nil {
		return err
	}
	creds.Sessions[baseURL] = credentialSession{Email: email, AccessToken: resp.AccessToken, RefreshToken: resp.RefreshToken}
	if err := saveCredentials(creds); err != nil {
		return err
	}
	fmt.Printf("Logged in to %s as %s\n", baseURL, email)
	return nil
}

const httpMethodPost = "POST"

func accessTokenForURL(ctx context.Context, baseURL string) (string, error) {
	creds, err := loadCredentials()
	if err != nil {
		return "", err
	}
	sess, ok := creds.Sessions[normalizeBaseURL(baseURL)]
	if !ok || (sess.AccessToken == "" && sess.RefreshToken == "") {
		return "", fmt.Errorf("not logged in to %s; run air login %s", baseURL, baseURL)
	}
	if sess.RefreshToken == "" {
		return sess.AccessToken, nil
	}
	var resp airlockv1.RefreshResponse
	if err := doProto(ctx, baseURL, httpMethodPost, "/auth/refresh", "", &airlockv1.RefreshRequest{RefreshToken: sess.RefreshToken}, &resp); err != nil {
		return "", fmt.Errorf("refresh login for %s: %w", baseURL, err)
	}
	sess.AccessToken = resp.AccessToken
	creds.Sessions[normalizeBaseURL(baseURL)] = sess
	if err := saveCredentials(creds); err != nil {
		return "", err
	}
	return sess.AccessToken, nil
}

func credentialsPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "airlock", "credentials.json"), nil
}

func loadCredentials() (credentialsFile, error) {
	path, err := credentialsPath()
	if err != nil {
		return credentialsFile{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return credentialsFile{Sessions: map[string]credentialSession{}}, nil
		}
		return credentialsFile{}, err
	}
	var creds credentialsFile
	if err := json.Unmarshal(b, &creds); err != nil {
		return credentialsFile{}, err
	}
	if creds.Sessions == nil {
		creds.Sessions = map[string]credentialSession{}
	}
	return creds, nil
}

func saveCredentials(creds credentialsFile) error {
	path, err := credentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o600)
}
