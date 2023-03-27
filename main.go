package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/lambda"
	"github.com/aws/aws-sdk-go/service/s3"

	. "github.com/logrusorgru/aurora"
)

type RcConfig struct {
	Name   string `json:"name"`
	Bucket string `json:"bucket"`
	Region string `json:"region"`
	Arm    bool   `json:"arm"`
}

func main() {
	// FLAGS =========================================================================================

	// Load flags
	functionPath := flag.String("path", ".", "Function path")
	functionName := flag.String("name", "", "Function name")
	s3Bucket := flag.String("bucket", "", "S3 bucket name")
	region := flag.String("region", "", "AWS Region")
	arm := flag.Bool("arm", false, "Build for aws Graviton2 arm arch")

	// Parse flags
	flag.Parse()

	// Get absolute path for lamlbda func
	realFunctionPath, err := filepath.Abs(*functionPath)
	if err != nil {
		fmt.Println(Red(err.Error()))
		os.Exit(1)
	}

	// Check dir exitence
	if _, err = os.Stat(realFunctionPath); os.IsNotExist(err) {
		fmt.Println(Red(fmt.Sprintf("Function path %s not exist", realFunctionPath)))
		os.Exit(1)
	}

	// Get region
	var awsRegion string
	if len(*region) > 0 {
		awsRegion = *region
	} else {
		awsRegion = os.Getenv("AWS_DEFAULT_REGION")
	}

	// PUSHRC ========================================================================================

	// Try to load .pushrc.json file in function
	rcFilePath := fmt.Sprintf("%s/.pushrc.json", realFunctionPath)
	var rcBytes []byte
	rcBytes, err = ioutil.ReadFile(rcFilePath)

	if err != nil {
		// fmt.Println(Red(err.Error()))
		Magenta(">> No rc file found")
	} else {
		// Load json
		rcConfig := &RcConfig{}
		err = json.Unmarshal(rcBytes, &rcConfig)

		if err != nil {
			fmt.Println(Red(err.Error()))
			os.Exit(1)
		}

		// Append name
		if len(rcConfig.Name) > 0 {
			if len(*functionName) < 1 {
				functionName = &rcConfig.Name
			}
		}

		// Append bucket
		if len(rcConfig.Bucket) > 0 {
			if len(*s3Bucket) < 1 {
				s3Bucket = &rcConfig.Bucket
			}
		}

		// Append region
		if len(rcConfig.Region) > 0 {
			if len(awsRegion) < 1 {
				awsRegion = rcConfig.Region
			}
		}

		// Set Arm build
		if rcConfig.Arm {
			arm = &rcConfig.Arm
		}
	}

	// VALIDATE ======================================================================================

	// Check function name
	if len(*functionName) < 1 {
		fmt.Println(Red("Function name required"))
		fmt.Fprintf(os.Stderr, "Usage : pushlambdafungo [options]\n")
		flag.PrintDefaults()
		os.Exit(1)
	}

	if len(awsRegion) < 1 {
		fmt.Println(Red("AWS region required"))
		fmt.Fprintf(os.Stderr, "Usage : pushlambdafungo [options]\n")
		flag.PrintDefaults()
	}

	// Log params
	usingArm := "No"
	if *arm {
		usingArm = "Yes"
	}
	fmt.Println(">> Run pushlambdafungo for function", Green(realFunctionPath))
	fmt.Println(">> Function name :", Green(*functionName))
	fmt.Println(">> Arm build (AWS Graviton2) :", Green(usingArm))
	fmt.Println(">> AWS Region :", Green(awsRegion))
	fmt.Println(">> S3 Bucket :", Green(*s3Bucket))

	// BUILD =========================================================================================

	fmt.Println(">> Building target function ...")

	// Cross compile function for lambda env
	compileCmd := exec.Command(
		"go",
		"build",
		"-trimpath",
		"-o",
		fmt.Sprintf("%s/main", realFunctionPath),
		fmt.Sprintf("%s/main.go", realFunctionPath),
	)

	// Append correct env
	compileCmd.Env = os.Environ()
	compileCmd.Env = append(compileCmd.Env, "GOOS=linux")

	// Disable CGO (seems to be enabled by default on go1.19)
	compileCmd.Env = append(compileCmd.Env, "CGO_ENABLED=0")

	// If arm
	if *arm {
		compileCmd.Env = append(compileCmd.Env, "GOARCH=arm64")
	} else {
		// Else x86_64
		compileCmd.Env = append(compileCmd.Env, "GOARCH=amd64")
	}

	var compileOut []byte
	compileOut, err = compileCmd.CombinedOutput()

	if err != nil {
		fmt.Println(err.Error())
		fmt.Println(string(compileOut))
		os.Exit(1)
	}

	fmt.Println(">> Build target function", Green("success"))

	// ZIP ===========================================================================================

	fmt.Println(">> Zip generated binary ...")

	// Create zip buffer
	zipBuf := new(bytes.Buffer)

	// Create zip writer
	zipWriter := zip.NewWriter(zipBuf)

	zipFileName := "main"
	if *arm {
		zipFileName = "bootstrap"
	}

	zipFile, _ := zipWriter.CreateHeader(&zip.FileHeader{
		CreatorVersion: 3 << 8,
		ExternalAttrs:  0o777 << 16,
		Name:           zipFileName,
		Method:         zip.Deflate,
	})

	if err != nil {
		fmt.Println(Red(err.Error()))
		os.Exit(1)
	}

	// Read compiled binary as array of byte
	binaryContent, err := ioutil.ReadFile(fmt.Sprintf("%s/main", realFunctionPath))
	if err != nil {
		fmt.Println(Red(err.Error()))
		os.Exit(1)
	}

	// Add compiled bynary content to zip file
	zipFile.Write(binaryContent)

	// Close zip writer
	zipWriter.Close()

	fmt.Println(">> Zip generated binary", Green("success"))

	// S3 UPLOAD =====================================================================================

	fmt.Println(">> Push zip archive to s3 bucket", *s3Bucket, "...")

	// Create AWS session
	sess := session.Must(session.NewSession(&aws.Config{
		Region: aws.String(awsRegion),
	}))

	// Create S3 Client
	s3Client := s3.New(sess)

	// Create file on disk TEST
	// ioutil.WriteFile(fmt.Sprintf("%s/main.zip", realFunctionPath), zipBuf.Bytes(), 0644)

	// Create io reader from zip buffer
	zipReader := bytes.NewReader(zipBuf.Bytes())

	// Put object params
	s3Input := &s3.PutObjectInput{
		Bucket: aws.String(*s3Bucket),
		Key:    aws.String(fmt.Sprintf("%s.zip", *functionName)),
		Body:   aws.ReadSeekCloser(zipReader),
	}

	// Create object in s3
	_, err = s3Client.PutObject(s3Input)

	if err != nil {
		fmt.Println(Red(err.Error()))
		os.Exit(1)
	}

	fmt.Println(">> Push zip archive to s3 bucket", *s3Bucket, Green("success"))

	// UPDATE FUNCTION CODE ==========================================================================

	fmt.Println(">> Update aws lambda function code ...")

	lambdaClient := lambda.New(sess)
	updateFuncCodeInput := &lambda.UpdateFunctionCodeInput{
		FunctionName: aws.String(*functionName),
		Publish:      aws.Bool(false),
		DryRun:       aws.Bool(false),
		S3Bucket:     aws.String(*s3Bucket),
		S3Key:        aws.String(fmt.Sprintf("%s.zip", *functionName)),
	}

	if *arm {
		updateFuncCodeInput.Architectures = aws.StringSlice([]string{"arm64"})
	} else {
		updateFuncCodeInput.Architectures = aws.StringSlice([]string{"x86_64"})
	}

	_, err = lambdaClient.UpdateFunctionCode(updateFuncCodeInput)

	if err != nil {
		fmt.Println(Red(err.Error()))
		os.Remove(fmt.Sprintf("%s/main", *functionPath))
		os.Exit(1)
	}

	fmt.Println(">> Update aws lambda function code", Green("success"))

	// REMOVE GENERATED BINARY =======================================================================

	err = os.Remove(fmt.Sprintf("%s/main", *functionPath))

	if err != nil {
		fmt.Println(Red(err.Error()))
		os.Exit(1)
	}

	// DONE ==========================================================================================

	fmt.Println(">> All done :)")
}
