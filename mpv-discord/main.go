package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/tnychn/mpv-discord/discordrpc"
	"github.com/tnychn/mpv-discord/mpvrpc"
)

var (
	client   *mpvrpc.Client
	presence *discordrpc.Presence
)

// urlCache maps thumbnail source (URL or local path) -> uploaded Litterbox URL,
// so we don't re-upload the same image on every 1-second poll tick.
var urlCache = map[string]string{}

func init() {
	log.SetOutput(os.Stdout)
	log.SetFlags(log.Lmsgprefix)

	client = mpvrpc.NewClient()
	presence = discordrpc.NewPresence(os.Args[2])
}

var currTime int64 = time.Now().Local().UnixMilli()

func refreshCurrTime() {
	currTime = time.Now().Local().UnixMilli()
}

// uploadToLitterbox uploads a local file path or remote https:// URL to
// Litterbox (litterbox.catbox.moe) with a 72h expiry. No account or API key
// needed. Returns a clean https://litter.catbox.moe/XXXXXX.jpg URL.
func uploadToLitterbox(imageSource string) (string, error) {
	var imageBytes []byte
	var err error

	imageSource = strings.TrimSpace(imageSource)
	if strings.HasPrefix(imageSource, "https://") {
		resp, err := http.Get(imageSource)
		if err != nil {
			return "", fmt.Errorf("failed to download image: %w", err)
		}
		defer resp.Body.Close()
		imageBytes, err = io.ReadAll(resp.Body)
		if err != nil {
			return "", fmt.Errorf("failed to read image body: %w", err)
		}
	} else {
		imageBytes, err = os.ReadFile(imageSource)
		if err != nil {
			return "", fmt.Errorf("failed to read local image: %w", err)
		}
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	writer.WriteField("reqtype", "fileupload")
	writer.WriteField("time", "72h")
	part, _ := writer.CreateFormFile("fileToUpload", "thumbnail.jpg")
	part.Write(imageBytes)
	writer.Close()

	req, _ := http.NewRequest("POST", "https://litterbox.catbox.moe/resources/internals/api.php", body)
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read Litterbox response: %w", err)
	}
	result := strings.TrimSpace(string(respBytes))
	log.Printf("(thumbnail): Litterbox response: %s", result)

	if !strings.HasPrefix(result, "https://") {
		return "", fmt.Errorf("Litterbox upload failed: %s", result)
	}
	return result, nil
}

// getThumbnailImageKey reads the thumbnail source set by discord.lua
// (either a YouTube https:// URL or a local file path set via user-data),
// uploads it to Litterbox, caches and returns the URL.
// Falls back to "mpv" asset key on any error.
func getThumbnailImageKey() string {
	source, err := client.GetPropertyString("user-data/discord-thumbnail")
	if err != nil {
		log.Printf("(thumbnail): property read error: %v", err)
		return "mpv"
	}
	source = strings.TrimSpace(source)
	// mpv sometimes returns the value JSON-encoded with surrounding quotes — strip them
	source = strings.Trim(source, "\"")
	source = strings.TrimSpace(source)
	if source == "" || source == "mpv" {
		return "mpv"
	}

	if cached, ok := urlCache[source]; ok {
		return cached
	}

	lbURL, err := uploadToLitterbox(source)
	if err != nil {
		log.Printf("(thumbnail): Litterbox upload failed: %v", err)
		return "mpv"
	}

	log.Printf("(thumbnail): %s -> %s", source, lbURL)
	urlCache[source] = lbURL
	return lbURL
}

func getActivity() (activity discordrpc.Activity, err error) {
	getProperty := func(key string) (prop interface{}) {
		prop, err = client.GetProperty(key)
		return
	}
	getPropertyString := func(key string) (prop string) {
		prop, err = client.GetPropertyString(key)
		return
	}

	// Large Image
	activity.LargeImageKey = getThumbnailImageKey()
	activity.LargeImageText = "mpv"
	if version := getPropertyString("mpv-version"); version != "" {
		activity.LargeImageText += " " + version[4:]
	}

	// Details
	activity.Details = getPropertyString("media-title")
	fileFormat := getPropertyString("file-format")
	metaTitle := getProperty("metadata/by-key/Title")
	metaArtist := getProperty("metadata/by-key/Artist")
	metaAlbum := getProperty("metadata/by-key/Album")
	if metaTitle != nil {
		activity.Details = metaTitle.(string)
	}

	// State
	if metaArtist != nil {
		activity.State += " by " + metaArtist.(string)
	}
	if metaAlbum != nil {
		activity.State += " on " + metaAlbum.(string)
	}
	if activity.State == "" {
		if aid, ok := getProperty("aid").(string); !ok || aid != "false" {
			activity.Type = 2
			activity.State += "Audio"
		}
		activity.State += "/"
		if vid, ok := getProperty("vid").(string); !ok || vid != "false" {
			activity.State += "Video"
			activity.Type = 3
		}
		activity.State += (": " + fileFormat)
	}

	// Small Image
	buffering := getProperty("paused-for-cache")
	pausing := getProperty("pause")
	loopingFile := getPropertyString("loop-file")
	loopingPlaylist := getPropertyString("loop-playlist")
	if buffering != nil && buffering.(bool) {
		activity.SmallImageKey = "buffer"
		activity.SmallImageText = "Buffering"
	} else if pausing != nil && pausing.(bool) {
		activity.SmallImageKey = "pause"
		activity.SmallImageText = "Paused"
	} else if loopingFile != "no" || loopingPlaylist != "no" {
		activity.SmallImageKey = "loop"
		activity.SmallImageText = "Looping"
	} else {
		activity.SmallImageKey = "play"
		activity.SmallImageText = "Playing"
	}
	if percentage := getProperty("percent-pos"); percentage != nil {
		activity.SmallImageText += fmt.Sprintf(" (%d%%)", int(percentage.(float64)))
	}
	if pcount := getProperty("playlist-count"); pcount != nil && int(pcount.(float64)) > 1 {
		if ppos := getProperty("playlist-pos-1"); ppos != nil {
			activity.SmallImageText += fmt.Sprintf(" [%d/%d]", int(ppos.(float64)), int(pcount.(float64)))
		}
	}

	// Timestamps
	_duration := getProperty("duration")
	durationMillis := int64(_duration.(float64))
	_timePos := getProperty("time-pos")
	timePosMills := int64(_timePos.(float64))

	startTimePos := currTime - (timePosMills * 1000)
	duration := startTimePos + (durationMillis * 1000)

	if pausing != nil && !pausing.(bool) {
		activity.Timestamps = &discordrpc.ActivityTimestamps{
			Start: startTimePos,
			End:   duration,
		}
		refreshCurrTime()
	}
	return
}

func openClient() {
	if err := client.Open(os.Args[1]); err != nil {
		log.Fatalln(err)
	}
	log.Println("(mpv-ipc): connected")
}

func openPresence() {
	for range time.Tick(500 * time.Millisecond) {
		if client.IsClosed() {
			return
		}
		if err := presence.Open(); err == nil {
			break
		}
	}
	log.Println("(discord-ipc): connected")
}

func main() {
	defer func() {
		if !client.IsClosed() {
			if err := client.Close(); err != nil {
				log.Fatalln(err)
			}
			log.Println("(mpv-ipc): disconnected")
		}
		if !presence.IsClosed() {
			if err := presence.Close(); err != nil {
				log.Fatalln(err)
			}
			log.Println("(discord-ipc): disconnected")
		}
	}()

	openClient()
	go openPresence()

	for range time.Tick(time.Second) {
		activity, err := getActivity()
		if err != nil {
			if errors.Is(err, syscall.EPIPE) {
				break
			} else if !errors.Is(err, io.EOF) {
				log.Println(err)
				continue
			}
		}
		if !presence.IsClosed() {
			go func() {
				if err = presence.Update(activity); err != nil {
					if errors.Is(err, syscall.EPIPE) {
						if err = presence.Close(); err != nil {
							log.Fatalln(err)
						}
						log.Println("(discord-ipc): reconnecting...")
						go openPresence()
					} else if !errors.Is(err, io.EOF) {
						log.Println(err)
					}
				}
			}()
		}
	}
}
