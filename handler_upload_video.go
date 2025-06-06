package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/database"
	"github.com/google/uuid"
)

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	const maxMemory = 10 << 30
	r.Body = http.MaxBytesReader(w, r.Body, maxMemory)

	// Parse the video ID from the URL path
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	// Check if the user is authenticated
	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	// Validate the JWT and get the user ID
	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	// Check if the video exists and if the user is the owner
	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video from database", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "User is not the owner of the video", nil)
		return
	}

	// Read the data
	videoFile, fileHeader, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse video form", err)
		return
	}
	defer videoFile.Close()

	// Check the media type of the uploaded file
	mediaType, _, err := mime.ParseMediaType(fileHeader.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Couldn't parse media type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid media type", err)
		return
	}

	// Create a temporary file to save the uploaded video
	tempFile, err := os.CreateTemp("", "tubely-upload.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't create temporary video file", err)
		return
	}
	defer os.Remove(tempFile.Name())
	defer tempFile.Close() // defer is LIFO, so the file will be closed before being removed

	_, err = io.Copy(tempFile, videoFile)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't save video", err)
		return
	}
	_, err = tempFile.Seek(0, io.SeekStart) // read the file again from the beginning
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't reset file pointer", err)
		return
	}

	// Get the aspect ratio of the video
	aspectRatio, err := getVideoAspectRatio(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't get video aspect ratio", err)
		return
	}
	directory := ""
	switch aspectRatio {
	case "9:16":
		directory = "portrait"
	case "16:9":
		directory = "landscape"
	default:
		directory = "other"
	}

	// Generate a random S3 file key
	s3FileKey := getAssetPath(mediaType)
	s3FileKey = filepath.Join(directory, s3FileKey)

	// Process the video for fast start
	processedFilePath, err := processVideoForFastStart(tempFile.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't process video for fast start", err)
		return
	}
	defer os.Remove(processedFilePath)

	// Open the processed video file
	processedFile, err := os.Open(processedFilePath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't open processed video file", err)
		return
	}
	defer processedFile.Close()

	// Create the S3 PutObjectInput
	s3PutObjectInput := &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &s3FileKey,
		Body:        processedFile,
		ContentType: &mediaType,
	}
	_, err = cfg.s3Client.PutObject(r.Context(), s3PutObjectInput)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't upload video to S3", err)
		return
	}

	// Update the video metadata in the database with the presigned URL
	s3VideoURL := fmt.Sprintf("%s,%s", cfg.s3Bucket, s3FileKey)
	video.VideoURL = &s3VideoURL
	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video in database", err)
		return
	}
	presignedVideo, err := cfg.dbVideoToSignedVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't convert video to signed video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, presignedVideo)
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe",
		"-v", "error",
		"-print_format", "json",
		"-select_streams", "v:0",
		"-show_streams",
		filePath,
	)
	cmd.Stdout = &bytes.Buffer{}

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("ffprobe command failed: %w", err)
	}

	var output struct {
		Streams []struct {
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}

	if err := json.Unmarshal(cmd.Stdout.(*bytes.Buffer).Bytes(), &output); err != nil {
		return "", fmt.Errorf("could not parse ffprobe output: %v", err)
	}

	if len(output.Streams) == 0 {
		return "", fmt.Errorf("no video streams found")
	}

	width := float64(output.Streams[0].Width)
	height := float64(output.Streams[0].Height)
	if height == 0 {
		return "", fmt.Errorf("video height is zero, cannot determine aspect ratio")
	}
	aspectRatio := width / height

	verticalAspectRatio := 9.0 / 16.0
	horizontalAspectRatio := 16.0 / 9.0
	aspectRatioTolerance := 0.05
	if aspectRatio >= verticalAspectRatio-aspectRatioTolerance && aspectRatio <= verticalAspectRatio+aspectRatioTolerance {
		return "9:16", nil
	}
	if aspectRatio >= horizontalAspectRatio-aspectRatioTolerance && aspectRatio <= horizontalAspectRatio+aspectRatioTolerance {
		return "16:9", nil
	}
	return "other", nil
}

func processVideoForFastStart(inputFilePath string) (string, error) {
	processedFilePath := fmt.Sprintf("%s.processing", inputFilePath)

	cmd := exec.Command("ffmpeg",
		"-i", inputFilePath,
		"-movflags", "faststart",
		"-codec", "copy",
		"-f", "mp4",
		processedFilePath,
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("couldn't process video: %s, %v", stderr.String(), err)
	}

	fileInfo, err := os.Stat(processedFilePath)
	if err != nil {
		return "", fmt.Errorf("couldn't stat processed file: %v", err)
	}
	if fileInfo.Size() == 0 {
		return "", fmt.Errorf("processed file is empty")
	}

	return processedFilePath, nil
}

func generatePresignedURL(s3Client *s3.Client, bucket, key string, expireTime time.Duration) (string, error) {
	presignClient := s3.NewPresignClient(s3Client)
	params := s3.GetObjectInput{
		Bucket: &bucket,
		Key:    &key,
	}
	presignedURL, err := presignClient.PresignGetObject(context.Background(), &params, s3.WithPresignExpires(expireTime))
	if err != nil {
		return "", fmt.Errorf("couldn't generate presigned URL: %w", err)
	}
	return presignedURL.URL, nil
}

func (cfg *apiConfig) dbVideoToSignedVideo(video database.Video) (database.Video, error) {
	if video.VideoURL == nil {
		return video, nil
	}

	urlSlice := strings.Split(*video.VideoURL, ",")
	if len(urlSlice) != 2 {
		return video, fmt.Errorf("video URL is not in the expected format: %s", *video.VideoURL)
	}

	videoBucket := urlSlice[0]
	videoKey := urlSlice[1]
	presignedURL, err := generatePresignedURL(cfg.s3Client, videoBucket, videoKey, 10*time.Minute)
	if err != nil {
		return video, fmt.Errorf("couldn't generate presigned URL for video: %w", err)
	}

	video.VideoURL = &presignedURL
	return video, nil
}
