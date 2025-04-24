package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/go-github/v45/github"
	"golang.org/x/oauth2"
)

// version will be set during the build process via -ldflags
var version = "dev"

// FileSource represents the source of a configuration file (GitHub or S3)
type FileSource string

const (
	SourceGitHub FileSource = "github"
	SourceS3     FileSource = "s3"
)

// S3FileConfig represents a file stored in an S3 bucket
type S3FileConfig struct {
	Bucket    string `json:"bucket"`
	Key       string `json:"key"`
	LocalPath string `json:"local_path"`
	Region    string `json:"region"`
}

// S3Config represents configuration for an S3 data source
type S3Config struct {
	Files           map[string]S3FileConfig `json:"files"`
	AccessKeyID     string                  `json:"access_key_id,omitempty"`
	SecretAccessKey string                  `json:"secret_access_key,omitempty"`
	SessionToken    string                  `json:"session_token,omitempty"`
	UseIAMRole      bool                    `json:"use_iam_role,omitempty"`
	Anonymous       bool                    `json:"anonymous,omitempty"`
	PollInterval    string                  `json:"poll_interval"`
	EnvFile         string                  `json:"env_file,omitempty"`
}

// FileConfig represents a file stored in a GitHub repository
type FileConfig struct {
	RepoPath  string `json:"repo_path"`
	LocalPath string `json:"local_path"`
	Branch    string `json:"branch,omitempty"` // Optional branch name
}

// Config represents the main configuration structure
type Config struct {
	Source          FileSource            `json:"source"`
	Owner           string                `json:"owner,omitempty"`
	Repo            string                `json:"repo,omitempty"`
	Files           map[string]FileConfig `json:"files,omitempty"`
	Token           string                `json:"token,omitempty"`
	PollInterval    string                `json:"poll_interval"`
	EnvFile         string                `json:"env_file,omitempty"` // Optional path to env file
	S3Configuration *S3Config             `json:"s3_configuration,omitempty"`
}

var remoteSHAPattern = regexp.MustCompile(`^# REMOTE_SHA:\s*([a-f0-9]+)\s*$`)

func main() {
	// Define command-line flags
	configFlag := flag.String("config", "", "Path to the configuration file")
	versionFlag := flag.Bool("version", false, "Print version information and exit")
	helpFlag := flag.Bool("h", false, "Display help information")
	flag.Parse()

	// Print version information and exit if requested
	if *versionFlag {
		fmt.Printf("Config Sync v%s\n", version)
		return
	}

	// Display help if requested
	if *helpFlag {
		fmt.Printf("Config Sync v%s\n\n", version)
		fmt.Println("Usage: sync [options]")
		fmt.Println("Options:")
		flag.PrintDefaults()
		return
	}

	// Log version information on startup
	log.Printf("Config Sync v%s starting up", version)

	// Determine configuration file path (precedence: command line > environment > default)
	configPath := "./sink-config/config.json" // Default path
	if envPath := os.Getenv("CONFIG_PATH"); envPath != "" {
		configPath = envPath // Override with environment variable if set
	}
	if *configFlag != "" {
		configPath = *configFlag // Override with command-line flag if set
	}

	// Load configuration from file
	config, err := loadConfig(configPath)
	if err != nil {
		log.Fatalf("Failed to load configuration: %v", err)
	}

	// Set default source if not specified (backward compatibility)
	if config.Source == "" {
		config.Source = SourceGitHub
	}

	if err := validateConfig(config); err != nil {
		log.Fatalf("Configuration error: %v", err)
	}

	// Setup context for all operations
	ctx := context.Background()

	// Parse polling interval
	pollInterval := getPollInterval(config)

	// Start monitoring in separate goroutines
	var wg sync.WaitGroup

	// Handle GitHub source if configured
	if config.Source == SourceGitHub {
		// Setup GitHub client
		ts := oauth2.StaticTokenSource(
			&oauth2.Token{AccessToken: config.Token},
		)
		tc := oauth2.NewClient(ctx, ts)
		client := github.NewClient(tc)

		log.Printf("Starting monitoring for GitHub repository %s/%s", config.Owner, config.Repo)

		for id, fileConfig := range config.Files {
			wg.Add(1)
			go monitorFile(ctx, client, config, id, fileConfig, pollInterval, &wg)
		}
	}

	// Handle S3 source if configured
	if config.Source == SourceS3 && config.S3Configuration != nil {
		// Use S3 poll interval if specified, otherwise use the main config one
		s3PollInterval := pollInterval
		if config.S3Configuration.PollInterval != "" {
			var err error
			s3PollInterval, err = time.ParseDuration(config.S3Configuration.PollInterval)
			if err != nil {
				log.Printf("Invalid S3 polling interval: %v, using default instead", err)
			}
		}

		log.Printf("Starting monitoring for S3 files")

		for id, fileConfig := range config.S3Configuration.Files {
			wg.Add(1)
			go monitorS3File(ctx, config.S3Configuration, id, fileConfig, s3PollInterval, &wg)
		}
	}

	wg.Wait()
}

// getPollInterval returns the poll interval from config or a default value
func getPollInterval(config *Config) time.Duration {
	if config.PollInterval == "" {
		config.PollInterval = "5m" // Default to 5 minutes if not specified
	}

	pollInterval, err := time.ParseDuration(config.PollInterval)
	if err != nil {
		log.Printf("Invalid polling interval: %v, using default 5m instead", err)
		return 5 * time.Minute
	}

	return pollInterval
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
	// Check if we have at least one source configured
	if config.Source == "" {
		// For backward compatibility, assume GitHub if source is not specified
		config.Source = SourceGitHub
	}

	// Validate based on the source type
	if config.Source == SourceGitHub {
		if config.Owner == "" {
			return fmt.Errorf("owner is required for GitHub source")
		}
		if config.Repo == "" {
			return fmt.Errorf("repo is required for GitHub source")
		}
		if len(config.Files) == 0 {
			return fmt.Errorf("at least one file must be configured for GitHub source")
		}
		if config.Token == "" {
			return fmt.Errorf("GitHub token is required for GitHub source")
		}
	} else if config.Source == SourceS3 {
		if config.S3Configuration == nil {
			return fmt.Errorf("s3_configuration is required for S3 source")
		}
		if len(config.S3Configuration.Files) == 0 {
			return fmt.Errorf("at least one file must be configured in s3_configuration.files")
		}

		// Check that either anonymous, useIAMRole, or credentials are specified
		if !config.S3Configuration.Anonymous && !config.S3Configuration.UseIAMRole &&
			(config.S3Configuration.AccessKeyID == "" || config.S3Configuration.SecretAccessKey == "") {
			return fmt.Errorf("for S3 source, either anonymous=true, use_iam_role=true, or both access_key_id and secret_access_key must be provided")
		}

		// Verify each S3 file has required fields
		for name, fileConfig := range config.S3Configuration.Files {
			if fileConfig.Bucket == "" {
				return fmt.Errorf("bucket is required for S3 file %s", name)
			}
			if fileConfig.Key == "" {
				return fmt.Errorf("key is required for S3 file %s", name)
			}
			if fileConfig.LocalPath == "" {
				return fmt.Errorf("local_path is required for S3 file %s", name)
			}
			if fileConfig.Region == "" {
				return fmt.Errorf("region is required for S3 file %s", name)
			}
		}
	} else {
		return fmt.Errorf("unknown source type: %s", config.Source)
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

// createS3Client creates an AWS S3 client based on the provided configuration
func createS3Client(ctx context.Context, s3Config *S3Config, region string) (*s3.Client, error) {
	var cfg aws.Config
	var err error

	if s3Config.Anonymous {
		// For anonymous/public access
		customResolver := aws.EndpointResolverWithOptionsFunc(func(service, region string, options ...interface{}) (aws.Endpoint, error) {
			return aws.Endpoint{
				URL: fmt.Sprintf("https://s3.%s.amazonaws.com", region),
			}, nil
		})

		cfg, err = config.LoadDefaultConfig(ctx,
			config.WithRegion(region),
			config.WithEndpointResolverWithOptions(customResolver),
			config.WithCredentialsProvider(aws.AnonymousCredentials{}),
		)
	} else if s3Config.UseIAMRole {
		// Use IAM role from the environment (EC2, ECS, Lambda, etc.)
		cfg, err = config.LoadDefaultConfig(ctx, config.WithRegion(region))
	} else {
		// Use explicit credentials
		creds := credentials.NewStaticCredentialsProvider(
			s3Config.AccessKeyID,
			s3Config.SecretAccessKey,
			s3Config.SessionToken,
		)

		cfg, err = config.LoadDefaultConfig(ctx,
			config.WithRegion(region),
			config.WithCredentialsProvider(creds),
		)
	}

	if err != nil {
		return nil, fmt.Errorf("failed to configure AWS SDK: %v", err)
	}

	return s3.NewFromConfig(cfg), nil
}

// syncS3File downloads a file from S3 and writes it to the local path
func syncS3File(ctx context.Context, s3Config *S3Config, fileID string, fileConfig S3FileConfig) (string, error) {
	// Create S3 client for the file's region
	s3Client, err := createS3Client(ctx, s3Config, fileConfig.Region)
	if err != nil {
		return "", fmt.Errorf("failed to create S3 client: %v", err)
	}

	// Read local file (if exists) to retrieve stored SHA256
	localContent, err := os.ReadFile(fileConfig.LocalPath)
	localContentStr := ""
	if err == nil {
		localContentStr = string(localContent)
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("failed to read local file: %v", err)
	}

	// Get file from S3
	input := &s3.GetObjectInput{
		Bucket: aws.String(fileConfig.Bucket),
		Key:    aws.String(fileConfig.Key),
	}

	result, err := s3Client.GetObject(ctx, input)
	if err != nil {
		return "", fmt.Errorf("failed to get S3 object: %v", err)
	}
	defer result.Body.Close()

	// Read the content from S3
	remoteContent, err := io.ReadAll(result.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read S3 content: %v", err)
	}

	// Calculate SHA256 hash of the content
	hasher := sha256.New()
	hasher.Write(remoteContent)
	remoteSHA256 := hex.EncodeToString(hasher.Sum(nil))

	// Check for stored remote SHA256 marker in local file
	var storedSHA256 string
	lines := strings.Split(localContentStr, "\n")
	if len(lines) > 0 {
		// Adjust regex pattern for SHA256
		sha256Pattern := regexp.MustCompile(`^# REMOTE_SHA256:\s*([a-f0-9]+)\s*$`)
		if matches := sha256Pattern.FindStringSubmatch(lines[0]); len(matches) == 2 {
			storedSHA256 = matches[1]
		}
	}

	// If the remote SHA256 is the same as the stored one, no update is needed
	if storedSHA256 == remoteSHA256 {
		return remoteSHA256, nil
	}

	// Prepend the remote SHA256 marker to the remote content
	finalContent := fmt.Sprintf("# REMOTE_SHA256: %s\n%s", remoteSHA256, string(remoteContent))

	// Ensure the local directory exists
	dir := filepath.Dir(fileConfig.LocalPath)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return "", fmt.Errorf("failed to create directory %s: %v", dir, err)
	}

	// Write the updated file
	if err := os.WriteFile(fileConfig.LocalPath, []byte(finalContent), 0644); err != nil {
		return "", fmt.Errorf("failed to write file: %v", err)
	}

	log.Printf("File %s updated with remote SHA256 %s", fileConfig.LocalPath, remoteSHA256)

	// If environment file is specified, apply template substitution
	if s3Config.EnvFile != "" {
		// Create a minimal Config with just the env file path
		tempConfig := &Config{
			EnvFile: s3Config.EnvFile,
		}
		if err := applyEnvTemplate(fileConfig.LocalPath, tempConfig); err != nil {
			return "", fmt.Errorf("failed to apply env template: %v", err)
		}
	}

	return remoteSHA256, nil
}

// monitorS3File monitors an S3 file for changes
func monitorS3File(ctx context.Context, s3Config *S3Config, fileID string, fileConfig S3FileConfig, pollInterval time.Duration, wg *sync.WaitGroup) {
	defer wg.Done()

	log.Printf("Starting monitoring for S3 file %s (s3://%s/%s)\n", fileID, fileConfig.Bucket, fileConfig.Key)
	var lastSHA256 string

	for {
		sha256, err := syncS3File(ctx, s3Config, fileID, fileConfig)
		if err != nil {
			log.Printf("[%s] Error syncing S3 file: %v", fileID, err)
		} else if sha256 != lastSHA256 {
			log.Printf("[%s] S3 file updated successfully. New SHA256: %s", fileID, sha256)
			lastSHA256 = sha256
		}

		time.Sleep(pollInterval)
	}
}
