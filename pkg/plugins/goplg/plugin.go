package goplg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/haapjari/glass/pkg/models"
	"github.com/haapjari/glass/pkg/utils"
	JSONParser "github.com/tidwall/gjson"
	"golang.org/x/oauth2"
	"gorm.io/gorm"
)

var (
	GITHUB_API_TOKEN                string = fmt.Sprintf("%v", utils.GetGithubApiToken())
	GITHUB_USERNAME                 string = fmt.Sprintf("%v", utils.GetGithubUsername())
	SOURCEGRAPH_GRAPHQL_API_BASEURL string = utils.GetSourceGraphGraphQlApiBaseurl()
	GITHUB_GRAPHQL_API_BASEURL      string = utils.GetGithubGraphQlApiBaseurl()
	REPOSITORY_API_BASEURL          string = utils.GetRepositoryApiBaseUrl()
)

type GoPlugin struct {
	GitHubApiToken string
	GitHubUsername string
	HttpClient     *http.Client
	Parser         *Parser
	DatabaseClient *gorm.DB
	GitHubClient   *http.Client
	MaxThreads     int
}

func NewGoPlugin(DatabaseClient *gorm.DB) *GoPlugin {
	g := new(GoPlugin)

	g.HttpClient = &http.Client{}
	g.HttpClient.Timeout = time.Minute * 10 // TODO: Environment Variable

	tokenSource := oauth2.StaticTokenSource(
		&oauth2.Token{AccessToken: GITHUB_API_TOKEN},
	)

	g.GitHubClient = oauth2.NewClient(context.Background(), tokenSource)
	g.DatabaseClient = DatabaseClient
	g.MaxThreads = 20

	g.Parser = NewParser()

	return g
}

func (g *GoPlugin) GetRepositoryMetadata(count int) {
	// g.fetchRepositories(count)
	// g.deleteDuplicateRepositories // TODO
	// g.enrichRepositoriesWithPrimaryData()

	// g.calculateSizeOfPrimaryRepositories()

	g.enrichWithLibraryData("")
}

// Enriches the metadata with "Original Codebase Size" variables.
func (g *GoPlugin) calculateSizeOfPrimaryRepositories() {
	// fetch all the repositories from the database.
	repositories := g.getAllRepositories()

	// calculate the amount of repositories, and save it to variable.
	len := len(repositories.RepositoryData)

	// append the https:// and .git prefix and postfix the RepositoryUrl variables.
	for i := 0; i < len; i++ {
		repositories.RepositoryData[i].RepositoryUrl = "https://" + repositories.RepositoryData[i].RepositoryUrl + ".git"
	}

	var (
		output string
		err    string
	)

	// Calculate the amount of code lines for each repository.
	for i := 0; i < len; i++ {
		// If the OriginalCodebaseSize variable is empty, analyze the repository.
		// Otherwise skip the repository, in order to avoid double analysis.
		if repositories.RepositoryData[i].OriginalCodebaseSize == "" {
			// Save the repository url and name to variables, for easier access and less repetition.
			url := repositories.RepositoryData[i].RepositoryUrl
			name := repositories.RepositoryData[i].RepositoryName

			// Clone the repository.
			output, err = runCommand("git", "clone", url)
			fmt.Println(output)
			if err != "" {
				fmt.Println(err)
			}

			// Run "gocloc" and calculate the amount of lines.
			lines := runGoCloc(name)

			// Copy the repository struct to a new variable.
			var repositoryStruct models.Repository

			// Find matching repository from the database.
			if err := g.DatabaseClient.Where("repository_name = ?", name).First(&repositoryStruct).Error; err != nil {
				utils.CheckErr(err)
			}

			// Update the OriginalCodebaseSize variable, with calculated value.
			repositoryStruct.OriginalCodebaseSize = strconv.Itoa(lines)

			// Update the database.
			g.DatabaseClient.Model(&repositoryStruct).Updates(repositoryStruct)

			// Delete the repository.
			output, err = runCommand("rm", "-rf", name)
			fmt.Println(output)
			if err != "" {
				fmt.Println(err)
			}
		}
	}
}

// Fetches initial metadata of the repositories. Crafts a SourceGraph GraphQL request, and
// parses the repository location to the database table.
func (g *GoPlugin) fetchRepositories(count int) {
	// TODO: Export this as a environment or configuration value.
	queryStr := "{search(query:\"lang:go +  AND select:repo AND repohasfile:go.mod AND count:" + strconv.Itoa(count) + "\", version:V2){results{repositories{name}}}}"

	rawReqBody := map[string]string{
		"query": queryStr,
	}

	// Parse Body to JSON
	jsonReqBody, err := json.Marshal(rawReqBody)
	utils.CheckErr(err)

	bytesReqBody := bytes.NewBuffer(jsonReqBody)

	// Craft a request
	request, err := http.NewRequest("POST", SOURCEGRAPH_GRAPHQL_API_BASEURL, bytesReqBody)
	request.Header.Set("Content-Type", "application/json")
	utils.CheckErr(err)

	// Execute request
	res, err := g.HttpClient.Do(request)
	utils.CheckErr(err)

	defer res.Body.Close()

	// Read all bytes from the response
	sourceGraphResponseBody, err := ioutil.ReadAll(res.Body)
	utils.CheckErr(err)

	// Parse bytes JSON.
	var jsonSourceGraphResponse SourceGraphResponse
	json.Unmarshal([]byte(sourceGraphResponseBody), &jsonSourceGraphResponse)

	// Write the response to Database.
	g.writeSourceGraphResponseToDatabase(len(jsonSourceGraphResponse.Data.Search.Results.Repositories), jsonSourceGraphResponse.Data.Search.Results.Repositories)
}

// Reads the repositories -tables values to memory, crafts a GitHub GraphQL requests of the
// repositories, and appends the database entries with Open Issue Count, Closed Issue Count,
// Commit Count, Original Codebase Size, Repository Type, Primary Language, Stargazers Count,
// Creation Date, License.
func (g *GoPlugin) enrichWithMetadata() {
	r := g.getAllRepositories()
	c := len(r.RepositoryData)

	var wg sync.WaitGroup

	// Semaphore is a safeguard to goroutines, to allow only "MaxThreads" run at the same time.
	semaphore := make(chan int, g.MaxThreads)

	for i := 0; i < c; i++ {
		semaphore <- 1
		wg.Add(1)

		go func(i int) {
			// Parse Owner and Name values from the Repository, which are used in the GraphQL query.
			owner, name := g.Parser.ParseRepository(r.RepositoryData[i].RepositoryUrl)

			queryStr := "{repository(owner: \"" + owner + "\", name: \"" + name + "\") {defaultBranchRef {target {... on Commit {history {totalCount}}}}openIssues: issues(states:OPEN) {totalCount}closedIssues: issues(states:CLOSED) {totalCount}languages {totalSize}stargazerCount licenseInfo {key}createdAt latestRelease{publishedAt} primaryLanguage{name}}}"

			rawGithubRequestBody := map[string]string{
				"query": queryStr,
			}

			// Parse body to JSON.
			jsonGithubRequestBody, err := json.Marshal(rawGithubRequestBody)
			utils.CheckErr(err)

			bytesReqBody := bytes.NewBuffer(jsonGithubRequestBody)

			// Craft a request.
			githubRequest, err := http.NewRequest("POST", GITHUB_GRAPHQL_API_BASEURL, bytesReqBody)
			if err != nil {
				log.Fatalln(err)
			}

			githubRequest.Header.Set("Accept", "application/vnd.github.v3+json")

			// Execute a request with Oauth2 client.
			githubResponse, err := g.GitHubClient.Do(githubRequest)
			utils.CheckErr(err)

			defer githubResponse.Body.Close()

			// Read the response bytes to a variable.
			githubResponseBody, err := ioutil.ReadAll(githubResponse.Body)
			utils.CheckErr(err)

			// Parse bytes to JSON.
			var jsonGithubResponse GitHubResponse
			json.Unmarshal([]byte(githubResponseBody), &jsonGithubResponse)

			var existingRepositoryStruct models.Repository

			// Search for existing model, which matches the id and copy the values to the "existingRepositoryStruct" variable.
			if err := g.DatabaseClient.Where("id = ?", r.RepositoryData[i].Id).First(&existingRepositoryStruct).Error; err != nil {
				utils.CheckErr(err)
			}

			// Create new struct, with updated values.
			var newRepositoryStruct models.Repository

			newRepositoryStruct.RepositoryName = name
			newRepositoryStruct.RepositoryUrl = r.RepositoryData[i].RepositoryUrl
			newRepositoryStruct.OpenIssueCount = strconv.Itoa(jsonGithubResponse.Data.Repository.OpenIssues.TotalCount)
			newRepositoryStruct.ClosedIssueCount = strconv.Itoa(jsonGithubResponse.Data.Repository.ClosedIssues.TotalCount)
			newRepositoryStruct.CommitCount = strconv.Itoa(jsonGithubResponse.Data.Repository.DefaultBranchRef.Target.History.TotalCount)
			newRepositoryStruct.RepositoryType = "primary"
			newRepositoryStruct.PrimaryLanguage = jsonGithubResponse.Data.Repository.PrimaryLanguage.Name
			newRepositoryStruct.CreationDate = jsonGithubResponse.Data.Repository.CreatedAt
			newRepositoryStruct.StargazerCount = strconv.Itoa(jsonGithubResponse.Data.Repository.StargazerCount)
			newRepositoryStruct.LicenseInfo = jsonGithubResponse.Data.Repository.LicenseInfo.Key
			newRepositoryStruct.LatestRelease = jsonGithubResponse.Data.Repository.LatestRelease.PublishedAt

			// Update the existing model, with values from the new struct.
			g.DatabaseClient.Model(&existingRepositoryStruct).Updates(newRepositoryStruct)

			defer func() { <-semaphore }()
		}(i)
		wg.Done()
	}

	wg.Wait()

	// When the Channel Length is not 0, there is still running Threads.
	for !(len(semaphore) == 0) {
		continue
	}
}

// TODO
func (g *GoPlugin) enrichWithLibraryData(url string) {
	// Query String
	// TODO: Replace URL from queryString with repositoryUrl
	queryString := "{repository(name: \"github.com/kubernetes/kubernetes\") {defaultBranch {target {commit {blob(path: \"go.mod\") {content}}}}}}"

	// Construct the Query
	rawRequestBody := map[string]string{
		"query": queryString,
	}

	// Parse Body from Map to JSON
	jsonRequestBody, err := json.Marshal(rawRequestBody)
	utils.CheckErr(err)

	// Convert the Body from JSON to Bytes
	requestBodyInBytes := bytes.NewBuffer(jsonRequestBody)

	// Craft a Request
	request, err := http.NewRequest("POST", SOURCEGRAPH_GRAPHQL_API_BASEURL, requestBodyInBytes)
	request.Header.Set("Content-Type", "application/json")
	utils.CheckErr(err)

	// Execute Request
	res, err := g.HttpClient.Do(request)
	utils.CheckErr(err)

	// Close the Body, after surrounding function returns.
	defer res.Body.Close()

	// Read all bytes from the response. (Empties the res.Body)
	sourceGraphResponseBody, err := ioutil.ReadAll(res.Body)
	utils.CheckErr(err)

	// Parse JSON with "https://github.com/buger/jsonparser"
	outerModFile := JSONParser.Get(string(sourceGraphResponseBody), "data.repository.defaultBranch.target.commit.blob.content")

	// Parsing of the inner modfiles.

	var (
		libraries     []string
		innerModFiles []string
		// totalCodeLines           int
	)

	if checkInnerModFiles(outerModFile.String()) {
		innerModFiles = parseInnerModFiles(outerModFile.String())
	}

	// Print the inner modfiles.
	// TODO: Remove
	for _, innerModFile := range innerModFiles {
		fmt.Println(innerModFile)
	}

	fmt.Println("---")

	// Parsing libraries from go.mod file.

	// Libraries are saved in a slice of "libraries"
	libraries = parseLibrariesFromModFile(outerModFile.String())

	// Printing
	// TODO: Remove
	for _, lib := range libraries {
		fmt.Println(lib)
	}

	fmt.Println("---")

	// // Parsing the outer modfile ends here.

	// TODO: This will happen in a loop.
	// Perform a GET request, to get the content of the inner modfile.

	// We can discard the replace contents of the inner modfiles, because they already
	// taken in account.

	// modFileContent := performGetRequest(innerModFiles[0])

	// modFileContentSlice := parseLibrariesFromModFile(modFileContent)

	// fmt.Println("---")

	// TODO: For some reason the parseLibrariesFromModFile is not working properly.
	// It's not taking in account the second parenthesis in modFileContent, why not (?)

	// Printing
	//	for _, lib := range modFileContentSlice {
	//fmt.Println(lib)
	//}

	//fmt.Println(modFileContent)

	// libraries = append(libraries, parseLibrariesFromModFile(modFileContent)...)

	// TODO: Parse the library names of the inner modfiles.
	// TODO: Create a cache, that holds the code lines of analyzed libraries.
	// TODO: Implement functionality to calculate code lines of the libraries.
	// TODO: Delete duplicates from the libraries slice.
}

func parseInnerModFiles(str string) []string {

	var (
		innerModFilesString string
		innerModFiles       []string
	)

	// If the project has inner modfiles, parse the locations to the variable innerModFilesSlice.
	innerModFiles = strings.Split(str, "replace")
	innerModFilesString = innerModFiles[1]

	// remove trailing '(' and ')' runes from the string
	innerModFilesString = utils.RemoveCharFromString(innerModFilesString, '(')
	innerModFilesString = utils.RemoveCharFromString(innerModFilesString, ')')

	// remove trailing whitespace,
	innerModFilesString = strings.TrimSpace(innerModFilesString)

	// split the strings from newline characters
	innerModFiles = strings.Split(innerModFilesString, "\n")

	// remove trailing whitespaces from the beginning and end of the elements in the string
	for i, line := range innerModFiles {
		innerModFiles[i] = strings.TrimSpace(line)
	}

	// remove characters until => occurence.
	for i, line := range innerModFiles {
		str := strings.Index(line, "=>")
		innerModFiles[i] = line[str+2:]
	}

	// TODO: Refactor prefix and postfix, with actual repository names.
	// add the "https://raw.githubusercontent.com/kubernetes/kubernetes/master" prefix and
	// "/go.mod" postfix to the string.
	prefix := "https://raw.githubusercontent.com/kubernetes/kubernetes/master"
	postfix := "go.mod"

	for i, line := range innerModFiles {
		// removing the dot from the beginning of the file
		line = strings.Replace(line, ".", "", 1)

		// remove whitespace inbetween the string
		line = strings.Replace(line, " ", "", -1)

		// concat the prefix and postfix
		line = prefix + line + "/" + postfix

		// modify the original slice
		innerModFiles[i] = line
	}

	return innerModFiles
}

// Check if the project contains inner modfiles.
func checkInnerModFiles(str string) bool {
	// replace -keyword means, that the project contains inner modfiles, boolean flag will be turned to true.
	if strings.Count(str, "replace") > 0 {
		return true
	}

	return false
}
