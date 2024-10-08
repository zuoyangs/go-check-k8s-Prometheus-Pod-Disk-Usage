package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/bndr/gotabulate"
)

const (
	webhookURL = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxxxxxxxxxxxxxxxxxxxxxxx"
)

type WeChatMessage struct {
	MsgType string `json:"msgtype"`
	Text    struct {
		Content string `json:"content"`
	} `json:"text"`
}

func sendToWeChat(webhookURL string, message WeChatMessage) error {
	messageBytes, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("无法序列化消息: %v", err)
	}

	resp, err := http.Post(webhookURL, "application/json", bytes.NewBuffer(messageBytes))
	if err != nil {
		return fmt.Errorf("发送消息失败: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("消息发送失败，状态码: %d", resp.StatusCode)
	}

	return nil
}

func atoi(s string) int {
	i, _ := strconv.Atoi(s)
	return i
}

func handleError(err error, msg string) {
	if err != nil {
		fmt.Printf("%s: %v\n", msg, err)
	}
}

// 执行kubectl命令
func runKubectlCommand(config, namespace, pod, container, command, duCommand string) ([]string, error) {
	kubectlArgs := []string{"exec", pod, "-n", namespace, "-c", container, "--", "/bin/sh", "-c", command}
	cmd := exec.Command("kubectl", kubectlArgs...)
	cmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG=%s", config))
	var out, stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("执行kubectl命令失败: %v\n错误输出: %s", err, stderr.String())
	}

	duArgs := []string{"exec", pod, "-n", namespace, "-c", container, "--", "/bin/sh", "-c", duCommand}
	duCmd := exec.Command("kubectl", duArgs...)
	duCmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG=%s", config))
	var duOut, duStderr bytes.Buffer
	duCmd.Stdout = &duOut
	duCmd.Stderr = &duStderr

	if err := duCmd.Run(); err != nil {
		return []string{config, pod, out.String(), ""}, fmt.Errorf("执行du命令失败: %v\n错误输出: %s", err, duStderr.String())
	}

	return []string{config, pod, out.String(), strings.TrimSpace(duOut.String())}, nil
}

// 获取Pod列表
func getPods(config, namespace string) ([]string, error) {
	cmd := exec.Command("kubectl", "get", "pods", "-n", namespace, "-o", "custom-columns=NAME:.metadata.name")
	cmd.Env = append(os.Environ(), fmt.Sprintf("KUBECONFIG=%s", config))
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("获取pod列表失败 for kubeconfig %s: %v", config, err)
	}

	re := regexp.MustCompile(`(^prometheus-k8s|^prometheus-istio)`)
	pods := strings.Split(out.String(), "\n")
	var filteredPods []string
	for _, pod := range pods {
		pod = strings.TrimSpace(pod)
		if re.MatchString(pod) {
			filteredPods = append(filteredPods, pod)
		}
	}

	return filteredPods, nil
}

// 获取磁盘使用情况
func getDiskUsage(config, pod string, wg *sync.WaitGroup, results chan<- []string) {
	defer wg.Done()

	command, duCommand := getCommands(config)
	result, err := runKubectlCommand(config, "monitoring", pod, "prometheus", command, duCommand)
	if err != nil {
		fmt.Printf("kubeconfig: %s, 获取磁盘使用情况失败: %v\n", config, err)
		results <- []string{config, pod, "", ""}
		return
	}

	results <- result
}

// 根据配置文件选择不同的命令
func getCommands(config string) (string, string) {
	switch config {
	case
		"/root/.kube/sys/putuo-pt-rke.yaml",
		"/root/.kube/sys/stage-rke.yaml",
		"/root/.kube/sys/z-prod-ack.yaml",
		"/root/.kube/sys/z-prod-tke.yaml":
		return "df -h | grep -w /prometheus | awk '{print $2, $3, $4, $5, $6}'", "du -sh /prometheus/ | awk '{print $1, $2}'"
	default:
		return "df -h | awk '{print $1, $2, $3, $4, $5, $6}' | grep -w /prometheus", "du -sh /prometheus/ | awk '{print $1, $2}'"
	}
}

// 过滤Pods
func filterPods(pods []string, podFilter *regexp.Regexp) []string {
	var filteredPods []string
	for _, pod := range pods {
		if podFilter.MatchString(pod) {
			filteredPods = append(filteredPods, pod)
		}
	}
	return filteredPods
}

func displayAndSendTable(tableData [][]string, webhookURL string) {
	t := gotabulate.Create(tableData)
	t.SetHeaders([]string{"KUBECONFIG", "POD", "(Size   Used    Avail   Use%) df -h", "du -sh /prometheus"})
	t.SetMaxCellSize(60)
	t.SetAlign("right")
	fmt.Println(t.Render("simple"))

	var resultString strings.Builder
	resultString.WriteString(t.Render("simple"))

	message := WeChatMessage{
		MsgType: "text",
		Text: struct {
			Content string `json:"content"`
		}{
			Content: resultString.String(),
		},
	}

	if err := sendToWeChat(webhookURL, message); err != nil {
		fmt.Printf("发送消息到企业微信失败: %v\n", err)
	} else {
		fmt.Println("消息已成功发送到企业微信")
	}
}

func main() {
	dir := "/root/.kube/sys"
	var wg sync.WaitGroup
	resultsChan := make(chan []string, 100)
	podFilter := regexp.MustCompile(`^(prometheus-k8s|prometheus-istio)`)
	var tableData [][]string

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		handleError(err, "遍历目录失败")
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".yaml") {
			config := path
			pods, err := getPods(config, "monitoring")
			handleError(err, fmt.Sprintf("获取pod列表失败, config: %s: %v", config, err))
			filteredPods := filterPods(pods, podFilter)
			for _, pod := range filteredPods {
				wg.Add(1)
				go getDiskUsage(config, pod, &wg, resultsChan)
			}
		}
		return nil
	})

	handleError(err, "遍历目录出错")
	wg.Wait()
	close(resultsChan)

	for result := range resultsChan {
		if len(result) > 0 && result[2] != "" && result[3] != "" {
			diskUsage := strings.Fields(result[2])
			if len(diskUsage) > 4 {
				diskUsage = diskUsage[len(diskUsage)-5:]
			}
			result[2] = strings.Join(diskUsage, " ")
			tableData = append(tableData, result)
		}
	}

	sort.Slice(tableData, func(i, j int) bool {
		percI := regexp.MustCompile(`\d+%`).FindString(tableData[i][2])
		percJ := regexp.MustCompile(`\d+%`).FindString(tableData[j][2])
		percIInt := atoi(percI[:len(percI)-1])
		percJInt := atoi(percJ[:len(percJ)-1])
		return percIInt > percJInt
	})

	displayAndSendTable(tableData, webhookURL)
}
