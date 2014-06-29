// Automatically downloads and configures Steam grid images for all games in a
// given Steam installation.
package main

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/draw"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/boppreh/go-ui"
	"image/jpeg"
	_ "image/png"
)

// User in the local steam installation.
type User struct {
	Name      string
	SteamId32 string
	SteamId64 string
	Dir       string
}

// Used to convert between SteamId32 and SteamId64.
const idConversionConstant = 76561197960265728

// Given the Steam installation dir (NOT the library!), returns all users in
// this computer.
func GetUsers(installationDir string) ([]User, error) {
	userdataDir := filepath.Join(installationDir, "userdata")
	files, err := ioutil.ReadDir(userdataDir)
	if err != nil {
		return nil, err
	}

	users := make([]User, 0)

	for _, userDir := range files {
		userId := userDir.Name()
		userDir := filepath.Join(userdataDir, userId)

		configFile := filepath.Join(userDir, "config", "localconfig.vdf")
		// Malformed user directory. Without the localconfig file we can't get
		// the username and the game list, so we skip it.
		if _, err := os.Stat(configFile); err != nil {
			continue
		}

		configBytes, err := ioutil.ReadFile(configFile)
		if err != nil {
			return nil, err
		}

		// Makes sure the grid directory exists.
		gridDir := filepath.Join(userDir, "config", "grid")
		err = os.MkdirAll(gridDir, 0777)
		if err != nil {
			return nil, err
		}

		// The Linux version of Steam ships with the "grid" dir without executable bit.
		// This in turn denies permission to everything inside the folder. This line is
		// here to ensure we have the correct permission.
		fmt.Println("Setting permission...")
		os.Chmod(gridDir, 0777)

		pattern := regexp.MustCompile(`"PersonaName"\s*"(.+?)"`)
		username := pattern.FindStringSubmatch(string(configBytes))[1]

		steamId32, err := strconv.ParseInt(userId, 10, 64)
		steamId64 := steamId32 + idConversionConstant
		strSteamId64 := strconv.FormatInt(steamId64, 10)
		users = append(users, User{username, userId, strSteamId64, userDir})
	}

	return users, nil
}

// URL to get the game list from the SteamId64.
const profilePermalinkFormat = `http://steamcommunity.com/profiles/%v/games?tab=all`

// The Steam website has the terrible habit of returning 200 OK when requests
// fail, and signaling the error in HTML. So we have to parse the request to
// check if it has failed, and cross our fingers that they don't change the
// message.
const steamProfileErrorMessage = `The specified profile could not be found.`

// Returns the HTML profile from a user from their SteamId32.
func GetProfile(user User) (string, error) {
	response, err := http.Get(fmt.Sprintf(profilePermalinkFormat, user.SteamId64))
	if err != nil {
		return "", err
	}

	if response.StatusCode >= 400 {
		return "", errors.New("Profile not found. Make sure you have a public Steam profile.")
	}

	contentBytes, err := ioutil.ReadAll(response.Body)
	response.Body.Close()
	if err != nil {
		return "", err
	}

	profile := string(contentBytes)
	if strings.Contains(profile, steamProfileErrorMessage) {
		return "", errors.New("Profile not found.")
	}

	return profile, nil
}

// A Steam game in a library. May or may not be installed.
type Game struct {
	// Official Steam id.
	Id string
	// Warning, may contain Unicode characters.
	Name string
	// Tags, including user-created category and Steam's "Favorite" tag.
	Tags []string
	// Path for the grid image.
	ImagePath string
	// Raw bytes of the encoded image (usually jpg).
	ImageBytes []byte
}

// Pattern of game declarations in the public profile. It's actually JSON
// inside Javascript, but this way is easier to extract.
const profileGamePattern = `\{"appid":\s*(\d+),\s*"name":\s*"(.+?)"`

// Returns all games from a given user, using both the public profile and local
// files to gather the data. Returns a map of game by ID.
func GetGames(user User) (games map[string]*Game, err error) {
	profile, err := GetProfile(user)
	if err != nil {
		return
	}

	// Fetch game list from public profile.
	pattern := regexp.MustCompile(profileGamePattern)
	games = make(map[string]*Game, 0)
	for _, groups := range pattern.FindAllStringSubmatch(profile, -1) {
		gameId := groups[1]
		gameName := groups[2]
		tags := []string{""}
		imagePath := ""
		games[gameId] = &Game{gameId, gameName, tags, imagePath, nil}
	}

	// Fetch game categories from local file.
	sharedConfFile := filepath.Join(user.Dir, "7", "remote", "sharedconfig.vdf")
	if _, err := os.Stat(sharedConfFile); err != nil {
		// No categories file found, skipping this part.
		return games, nil
	}
	sharedConfBytes, err := ioutil.ReadFile(sharedConfFile)

	sharedConf := string(sharedConfBytes)
	// VDF patterN: "steamid" { "tags { "0" "category" } }
	gamePattern := regexp.MustCompile(`"([0-9]+)"\s*{[^}]+?"tags"\s*{([^}]+?)}`)
	tagsPattern := regexp.MustCompile(`"[0-9]+"\s*"(.+?)"`)
	for _, gameGroups := range gamePattern.FindAllStringSubmatch(sharedConf, -1) {
		gameId := gameGroups[1]
		tagsText := gameGroups[2]

		for _, tagGroups := range tagsPattern.FindAllStringSubmatch(tagsText, -1) {
			tag := tagGroups[1]

			game, ok := games[gameId]
			if ok {
				game.Tags = append(game.Tags, tag)
			} else {
				// If for some reason it wasn't included in the profile, create a new
				// entry for it now. Unfortunately we don't have a name.
				gameName := ""
				games[gameId] = &Game{gameId, gameName, []string{tag}, "", nil}
			}
		}
	}

	return
}

// When all else fails, Google it. Unfortunately this is a deprecated API and
// may go offline at any time. Because this is last resort the number of
// requests shouldn't trigger any punishment.
const googleSearchFormat = `https://ajax.googleapis.com/ajax/services/search/images?v=1.0&rsz=8&q=`

// Returns the first steam grid image URL found by Google search of a given
// game name.
func getGoogleImage(gameName string) (string, error) {
	if gameName == "" {
		return "", nil
	}

	url := googleSearchFormat + url.QueryEscape("steam grid OR header"+gameName)
	response, err := http.Get(url)
	if err != nil {
		return "", err
	}

	responseBytes, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return "", err
	}
	response.Body.Close()
	// Again, we could parse JSON. This may be a little too lazy, the pattern
	// is very loose. The order could be wrong, for example.
	pattern := regexp.MustCompile(`"width":"460","height":"215",[^}]+"unescapedUrl":"(.+?)"`)
	matches := pattern.FindStringSubmatch(string(responseBytes))
	if len(matches) >= 1 {
		return matches[1], nil
	} else {
		return "", nil
	}
}

// Tries to fetch a URL, returning the response only if it was positive.
func tryDownload(url string) (*http.Response, error) {
	response, err := http.Get(url)
	if err != nil {
		return nil, err
	}

	if response.StatusCode == 404 {
		// Some apps don't have an image and there's nothing we can do.
		return nil, nil
	} else if response.StatusCode > 400 {
		// Other errors should be reported, though.
		return nil, errors.New("Failed to download image " + url + ": " + response.Status)
	}

	return response, nil
}

// Primary URL for downloading grid images.
const akamaiUrlFormat = `https://steamcdn-a.akamaihd.net/steam/apps/%v/header.jpg`

// The subreddit mentions this as primary, but I've found Akamai to contain
// more images and answer faster.
const steamCdnUrlFormat = `http://cdn.steampowered.com/v/gfx/apps/%v/header.jpg`

// Tries to load the grid image for a game from a number of alternative
// sources. Returns the final response received and a flag indicating if it was
// from a Google search (useful because we want to log the lower quality
// images).
func getImageAlternatives(game *Game) (response *http.Response, fromSearch bool, err error) {
	response, err = tryDownload(fmt.Sprintf(akamaiUrlFormat, game.Id))
	if err == nil && response != nil {
		return
	}

	response, err = tryDownload(fmt.Sprintf(steamCdnUrlFormat, game.Id))
	if err == nil && response != nil {
		return
	}

	fromSearch = true
	url, err := getGoogleImage(game.Name)
	if err != nil {
		return
	}
	response, err = tryDownload(url)
	if err == nil && response != nil {
		return
	}

	return nil, false, nil
}

// Downloads the grid image for a game into the user's grid directory. Returns
// flags indicating if the operation succeeded and if the image downloaded was
// from a search.
func DownloadImage(game *Game, user User) (found bool, fromSearch bool, err error) {
	gridDir := filepath.Join(user.Dir, "config", "grid")
	filename := filepath.Join(gridDir, game.Id+".jpg")
	backupFilename := filepath.Join(gridDir, game.Id+" (original).jpg")

	game.ImagePath = filename
	if _, err := os.Stat(backupFilename); err == nil {
		imageBytes, err := ioutil.ReadFile(backupFilename)
		game.ImageBytes = imageBytes
		return true, false, err
	}

	if _, err := os.Stat(filename); err == nil {
		// There's an image, but not a backup. Load the image and back it up.
		imageBytes, err := ioutil.ReadFile(filename)
		if err != nil {
			return true, false, err
		}

		game.ImageBytes = imageBytes
		return true, false, ioutil.WriteFile(backupFilename, game.ImageBytes, 0666)
	}

	response, fromSearch, err := getImageAlternatives(game)
	if response == nil || err != nil {
		return false, false, err
	}

	imageBytes, err := ioutil.ReadAll(response.Body)
	game.ImageBytes = imageBytes
	response.Body.Close()
	err = ioutil.WriteFile(filename, game.ImageBytes, 0666)
	if err != nil {
		return true, false, err
	}
	return true, fromSearch, ioutil.WriteFile(backupFilename, game.ImageBytes, 0666)
}

// Loads an image from a given path.
func loadImage(path string) (img image.Image, err error) {
	reader, err := os.Open(path)
	if err != nil {
		return
	}
	defer reader.Close()
	
	img, _, err = image.Decode(reader)
	return
}

// Loads the overlays from the given dir, returning a map of name -> image.
func LoadOverlays(dir string) (overlays map[string]image.Image, err error) {
	overlays = make(map[string]image.Image, 0)

	if _, err = os.Stat(dir); err != nil {
		return overlays, nil
	}

	files, err := ioutil.ReadDir(dir)
	if err != nil {
		return
	}

	for _, file := range files {
		img, err := loadImage(filepath.Join(dir, file.Name()))
		if err != nil {
			return overlays, err
		}

		name := strings.TrimSuffix(file.Name(), filepath.Ext(file.Name()))
		// Normalize overlay name.
		name = strings.TrimRight(strings.ToLower(name), "s")
		overlays[name] = img
	}

	return
}

// Applies an overlay to the game image, depending on the category. The
// resulting image is saved over the original.
func ApplyOverlay(game *Game, overlays map[string]image.Image) (err error) {
	if game.ImagePath == "" || game.ImageBytes == nil {
		return nil
	}

	for _, tag := range game.Tags {
		// Normalize tag name by lower-casing it and remove trailing "s".
		tagName := strings.TrimRight(strings.ToLower(tag), "s")

		overlayImage, ok := overlays[tagName]
		if !ok {
			continue
		}

		gameImage, _, err := image.Decode(bytes.NewBuffer(game.ImageBytes))
		if err != nil {
			return err
		}

		result := image.NewRGBA(gameImage.Bounds().Union(overlayImage.Bounds()))
		draw.Draw(result, result.Bounds(), gameImage, image.ZP, draw.Src)
		draw.Draw(result, result.Bounds(), overlayImage, image.Point{0, 0}, draw.Over)

		buf := new(bytes.Buffer)
		err = jpeg.Encode(buf, result, &jpeg.Options{90})
		if err != nil {
			return err
		}
		game.ImageBytes = buf.Bytes()
	}

	return ioutil.WriteFile(game.ImagePath, game.ImageBytes, 0666)
}

// Returns the Steam installation directory in Windows. Should work for
// internationalized systems, 32 and 64 bits and users that moved their
// ProgramFiles folder. If a folder is given by program parameter, uses that.
func GetSteamInstallation() (path string, err error) {
	if len(os.Args) == 2 {
		argDir := os.Args[1]
		_, err := os.Stat(argDir)
		if err == nil {
			return argDir, nil
		} else {
			return "", errors.New("Argument must be a valid Steam directory, or empty for auto detection. Got: " + argDir)
		}
	}

	currentUser, err := user.Current()
	if err == nil {
		linuxSteamDir := filepath.Join(currentUser.HomeDir, ".local", "share", "Steam")
		if _, err = os.Stat(linuxSteamDir); err == nil {
			return linuxSteamDir, nil
		}

		linuxSteamDir = filepath.Join(currentUser.HomeDir, ".steam", "steam")
		if _, err = os.Stat(linuxSteamDir); err == nil {
			return linuxSteamDir, nil
		}
	}

	programFiles86Dir := filepath.Join(os.Getenv("ProgramFiles(x86)"), "Steam")
	if _, err = os.Stat(programFiles86Dir); err == nil {
		return programFiles86Dir, nil
	}

	programFilesDir := filepath.Join(os.Getenv("ProgramFiles"), "Steam")
	if _, err = os.Stat(programFilesDir); err == nil {
		return programFilesDir, nil
	}

	return "", errors.New("Could not find Steam installation folder. You can drag and drop the Steam folder into `steamgrid.exe` for a manual override.")
}

// Prints an error and quits.
func errorAndExit(err error) {
	goui.Error("An unexpected error occurred:", err.Error())
	os.Exit(1)
}

func main() {
	goui.Start(func() {
		http.DefaultTransport.(*http.Transport).ResponseHeaderTimeout = time.Second * 10

		descriptions := make(chan string)
		progress := make(chan int)

		go goui.Progress("SteamGrid", descriptions, progress, func() { os.Exit(1) })

		startApplication(descriptions, progress)
	})
}

func startApplication(descriptions chan string, progress chan int) {
	descriptions <- "Loading overlays..."
	overlays, err := LoadOverlays(filepath.Join(filepath.Dir(os.Args[0]), "overlays by category"))
	if err != nil {
		errorAndExit(err)
	}
	if len(overlays) == 0 {
		// I'm trying to use a message box here, but for some reason the
		// message appears twice and there's an error a closed channel.
		fmt.Println("No overlays", "No category overlays found. You can put overlay images in the folder 'overlays by category', where the filename is the game category.\n\nContinuing without overlays...")
	}

	descriptions <- "Looking for Steam directory..."
	installationDir, err := GetSteamInstallation()
	if err != nil {
		errorAndExit(err)
	}

	descriptions <- "Loading users..."
	users, err := GetUsers(installationDir)
	if err != nil {
		errorAndExit(err)
	}
	if len(users) == 0 {
		errorAndExit(errors.New("No users found at Steam/userdata. Have you used Steam before in this computer?"))
	}

	notFounds := make([]*Game, 0)
	searchFounds := make([]*Game, 0)

	for _, user := range users {
		descriptions <- "Loading games for " + user.Name

		games, err := GetGames(user)
		if err != nil {
			errorAndExit(err)
		}

		i := 0
		for _, game := range games {
			fmt.Println(game.Name)

			i++
			progress <- i * 100 / len(games)
			var name string
			if game.Name != "" {
				name = game.Name
			} else {
				name = "unknown game with id " + game.Id
			}
			descriptions <- fmt.Sprintf("Processing %v (%v/%v)",
				name, i, len(games))

			found, fromSearch, err := DownloadImage(game, user)
			if err != nil {
				errorAndExit(err)
			}
			if !found {
				notFounds = append(notFounds, game)
				continue
			}
			if fromSearch {
				searchFounds = append(searchFounds, game)
			}

			err = ApplyOverlay(game, overlays)
			if err != nil {
				errorAndExit(err)
			}
		}

	}

	close(progress)

	message := ""
	if len(notFounds) == 0 && len(searchFounds) == 0 {
		message += "All grid images downloaded and overlays applied!\n\n"
	} else {
		if len(searchFounds) >= 1 {
			message += fmt.Sprintf("%v images were found with a Google search and may not be accurate:\n", len(searchFounds))
			for _, game := range searchFounds {
				message += fmt.Sprintf("* %v (steam id %v)\n", game.Name, game.Id)
			}

			message += "\n\n"
		}

		if len(notFounds) >= 1 {
			message += fmt.Sprintf("%v images could not be found anywhere:\n", len(notFounds))
			for _, game := range notFounds {
				message += fmt.Sprintf("* %v (steam id %v)\n", game.Name, game.Id)
			}

			message += "\n\n"
		}
	}
	message += "Open Steam in grid view to see the results!"

	goui.Info("Results", message)
}
