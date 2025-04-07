package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v45/github"
	"golang.org/x/oauth2"
)

type FileConfig struct {
	RepoPath  string `json:"repo_path"`
	LocalPath string `json:"local_path"`
	Branch    string `json:"branch,omitempty"` // Optional branch name
}

type Config struct {
	Owner        string                `json:"owner"`
	Repo         string                `json:"repo"`
	Files        map[string]FileConfig `json:"files"`
	Token        string                `json:"token"`
	PollInterval string                `json:"poll_interval"`
	EnvFile      string                `json:"env_file,omitempty"` // Optional path to env file
}

var remoteSHAPattern = regexp.MustCompile(`^# REMOTE_SHA:\s*([a-f0-9]+)\s*$`)

func main() {
	// Load configuration from file
	configPath := getEnvOrDefault("CONFIG_PATH", "./sync-config/config.json")
	config, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Parse polling interval
	pollInterval, err := time.ParseDuration(config.PollInterval)
	if err != nil {
		log.Fatalf("Invalid polling interval: %v", err)
	}

	if err := validateConfig(config); err != nil {
		log.Fatalf("Configuration error: %v", err)
	}

	// Setup GitHub client
	ctx := context.Background()
	ts := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: config.Token},
	)
	tc := oauth2.NewClient(ctx, ts)
	client := github.NewClient(tc)

	// Start monitoring each file in a separate goroutine
	var wg sync.WaitGroup
	for id, fileConfig := range config.Files {
		wg.Add(1)
		go monitorFile(ctx, client, config, id, fileConfig, pollInterval, &wg)
	}

	wg.Wait()
}

func monitorFile(ctx context.Context, client *github.Client, config *Config, fileID string, fileConfig FileConfig, pollInterval time.Duration, wg *sync.WaitGroup) {
	defer wg.Done()

	log.Printf("Starting monitoring for file %s (%s)\n", fileID, fileConfig.RepoPath)
	var lastSHA string

	for {
		sha, err := syncFile(ctx, client, config, fileConfig)
		if err != nil {
			log.Printf("[%s] Error syncing file: %v", fileID, err)
		} else if sha != lastSHA {
			log.Printf("[%s] File updated successfully. New SHA: %s", fileID, sha)
			lastSHA = sha
		}

		time.Sleep(pollInterval)
	}
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %v", err)
	}

	var config Config
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %v", err)
	}

	return &config, nil
}

func validateConfig(config *Config) error {
	if config.Owner == "" {
		return fmt.Errorf("owner is required in config")
	}
	if config.Repo == "" {
		return fmt.Errorf("repo is required in config")
	}
	if len(config.Files) == 0 {
		return fmt.Errorf("at least one file must be configured")
	}
	if config.Token == "" {
		return fmt.Errorf("GitHub token is required in config")
	}
	return nil
}

func syncFile(ctx context.Context, client *github.Client, config *Config, fileConfig FileConfig) (string, error) {
	// Read local file (if exists) to retrieve stored SHA.
	localContent, err := os.ReadFile(fileConfig.LocalPath)
	localContentStr := ""
	if err == nil {
		localContentStr = string(localContent)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("failed to read local file: %v", err)
	}

	// Prepare options for fetching remote content.
	opts := &github.RepositoryContentGetOptions{}
	if fileConfig.Branch != "" {
		opts.Ref = fileConfig.Branch
	}

	// Get remote content from GitHub.
	fileContent, _, _, err := client.Repositories.GetContents(
		ctx,
		config.Owner,
		config.Repo,
		fileConfig.RepoPath,
		opts,
	)
	if err != nil {
		return "", fmt.Errorf("failed to get remote content: %v", err)
	}

	remoteContent, err := fileContent.GetContent()
	if err != nil {
		return "", fmt.Errorf("failed to decode remote content: %v", err)
	}
	remoteSHA := *fileContent.SHA

	// Check for stored remote SHA marker in local file.
	var storedSHA string
	lines := strings.Split(localContentStr, "\n")
	if len(lines) > 0 {
		if matches := remoteSHAPattern.FindStringSubmatch(lines[0]); len(matches) == 2 {
			storedSHA = matches[1]
		}
	}

	// If the remote SHA is the same as the stored one, no update is needed.
	if storedSHA == remoteSHA {
		return remoteSHA, nil
	}

	// Prepend the remote SHA marker to the remote content.
	finalContent := fmt.Sprintf("# REMOTE_SHA: %s\n%s", remoteSHA, remoteContent)

	// Ensure the local directory exists.
	dir := filepath.Dir(fileConfig.LocalPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create directory %s: %v", dir, err)
	}

	// Write the updated file.
	if err := os.WriteFile(fileConfig.LocalPath, []byte(finalContent), 0644); err != nil {
		return "", fmt.Errorf("failed to write file: %v", err)
	}

	log.Printf("File %s updated with remote SHA %s", fileConfig.LocalPath, remoteSHA)

	// Trigger template substitution: update templated values in the file with values from the env file.
	if err := applyEnvTemplate(fileConfig.LocalPath, config); err != nil {
		return "", fmt.Errorf("failed to apply env template: %v", err)
	}

	return remoteSHA, nil
}

// applyEnvTemplate reads the env file and replaces occurrences of {{KEY}} in the given file with
// their corresponding values. It supports fallback values in the format {{KEY:-DEFAULT}}.
func applyEnvTemplate(filePath string, config *Config) error {
	// Determine env file path from config, environment variable, or default
	envFile := config.EnvFile
	if envFile == "" {
		envFile = getEnvOrDefault("ENV_FILE", "./env")
	}

	// If the env file doesn't exist, just skip replacement.
	if _, err := os.Stat(envFile); os.IsNotExist(err) {
		log.Printf("Environment file %s not found, skipping template substitution", envFile)
		return nil
	}

	envContent, err := os.ReadFile(envFile)
	if err != nil {
		return fmt.Errorf("failed to read env file: %v", err)
	}

	// Parse the environment file into a map.
	envMap := make(map[string]string)
	lines := strings.Split(string(envContent), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		// Remove quotes if present (e.g., "value" -> value)
		value = strings.Trim(value, `"'`)

		// Also check if the value is in the environment variables
		if envValue := os.Getenv(key); envValue != "" {
			value = envValue
		}

		envMap[key] = value
	}

	// Read the content of the file to be templated
	fileData, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read file for template substitution: %v", err)
	}
	contentStr := string(fileData)

	// Regular expression to match {{KEY}} or {{KEY:-DEFAULT}}
	re := regexp.MustCompile(`{{([^{}:]+)(?::-([^{}]*))?}}`)

	// Find all matches and replace them
	contentStr = re.ReplaceAllStringFunc(contentStr, func(match string) string {
		submatch := re.FindStringSubmatch(match)
		key := submatch[1]
		defaultValue := ""
		if len(submatch) > 2 {
			defaultValue = submatch[2]
		}

		// Check if the key exists in our environment map
		value, exists := envMap[key]
		if !exists {
			// If not in our map, check system environment variables
			value = os.Getenv(key)
		}

		// If still no value, use the default
		if value == "" {
			value = defaultValue
		}

		return value
	})

	// Write the updated content back to the file.
	if err := os.WriteFile(filePath, []byte(contentStr), 0644); err != nil {
		return fmt.Errorf("failed to write file after template substitution: %v", err)
	}

	log.Printf("Applied template substitutions from %s to %s", envFile, filePath)
	return nil
}

func getEnvOrDefault(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}
