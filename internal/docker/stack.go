package docker

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var exposeRe = regexp.MustCompile(`(?mi)^\s*EXPOSE\s+(\d{2,5})`)

// prepareDockerfile готовит Dockerfile в каталоге репозитория. Если он уже есть, используется как есть,
// а порт берётся из первой строки EXPOSE. Иначе стек определяется по файлам и Dockerfile создаётся.
// Возвращает стек, порт приложения и признак, что Dockerfile создан автоматически.
func prepareDockerfile(dir string) (stack string, port int, generated bool, err error) {
	if raw, e := os.ReadFile(filepath.Join(dir, "Dockerfile")); e == nil {
		port = 3000
		if m := exposeRe.FindSubmatch(raw); m != nil {
			if p, e := strconv.Atoi(string(m[1])); e == nil && p > 0 && p < 65536 {
				port = p
			}
		}
		return "Dockerfile", port, false, nil
	}

	stack, port, content, err := detectStack(dir)
	if err != nil {
		return "", 0, false, err
	}
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(content), 0o644); err != nil {
		return "", 0, false, err
	}
	// Служебные каталоги не нужны в образе и ускоряют сборку.
	_ = os.WriteFile(filepath.Join(dir, ".dockerignore"), []byte(".git\nnode_modules\n.next\n__pycache__\n.env\n"), 0o644)
	return stack, port, true, nil
}

func exists(dir, name string) bool {
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}

func detectStack(dir string) (stack string, port int, dockerfile string, err error) {
	if raw, e := os.ReadFile(filepath.Join(dir, "package.json")); e == nil {
		var pkg struct {
			Main         string            `json:"main"`
			Scripts      map[string]string `json:"scripts"`
			Dependencies map[string]string `json:"dependencies"`
		}
		if e := json.Unmarshal(raw, &pkg); e != nil {
			return "", 0, "", errors.New("package.json не читается")
		}
		install := "RUN if [ -f package-lock.json ]; then npm ci; else npm install; fi\n"
		if _, ok := pkg.Dependencies["next"]; ok {
			return "Next.js", 3000, "FROM node:22-alpine\nWORKDIR /app\nCOPY package*.json ./\n" + install +
				"COPY . .\nRUN npm run build\nENV NODE_ENV=production\nUSER node\nEXPOSE 3000\nCMD [\"npm\", \"start\"]\n", nil
		}
		cmd := `["npm", "start"]`
		if _, ok := pkg.Scripts["start"]; !ok {
			entry := pkg.Main
			if entry == "" {
				entry = "index.js"
			}
			if !exists(dir, entry) || strings.ContainsAny(entry, `"\`) {
				return "", 0, "", errors.New("в package.json нет скрипта start и не найден главный файл")
			}
			cmd = `["node", "` + entry + `"]`
		}
		return "Node.js", 3000, "FROM node:22-alpine\nWORKDIR /app\nCOPY package*.json ./\n" + install +
			"COPY . .\nRUN npm run build --if-present\nENV NODE_ENV=production\nUSER node\nEXPOSE 3000\nCMD " + cmd + "\n", nil
	}

	if exists(dir, "requirements.txt") || exists(dir, "pyproject.toml") {
		entry := ""
		for _, f := range []string{"main.py", "app.py", "server.py"} {
			if exists(dir, f) {
				entry = f
				break
			}
		}
		if entry == "" {
			return "", 0, "", errors.New("не найден файл запуска (main.py, app.py или server.py)")
		}
		install := "RUN pip install --no-cache-dir -r requirements.txt\n"
		copyReq := "COPY requirements.txt ./\n"
		if !exists(dir, "requirements.txt") {
			install, copyReq = "RUN pip install --no-cache-dir .\n", "COPY . .\n"
		}
		return "Python", 8000, "FROM python:3.12-slim\nWORKDIR /app\nENV PYTHONUNBUFFERED=1\n" + copyReq + install +
			"COPY . .\nRUN useradd -m app\nUSER app\nEXPOSE 8000\nCMD [\"python\", \"" + entry + "\"]\n", nil
	}
	return "", 0, "", errors.New("не удалось определить стек: добавьте Dockerfile в репозиторий")
}
