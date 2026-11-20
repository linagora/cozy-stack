package mails

import (
	"bytes"
	"io"
	"os"

	"github.com/cozy/cozy-stack/model/job"
	"github.com/cozy/cozy-stack/pkg/assets"
	"github.com/cozy/cozy-stack/pkg/config/config"
	"github.com/cozy/cozy-stack/pkg/utils"
	"github.com/cozy/cozy-stack/worker/exec"
	"github.com/spf13/afero"
)

func execMjml(ctx *job.TaskContext, template []byte, logErrors bool) ([]byte, error) {
	log := ctx.Logger()

	workDir, cleanDir, err := prepareWorkDir()
	defer cleanDir()
	if err != nil {
		if logErrors {
			log.Errorf("PrepareWorkDir: %s", err)
		}
		return nil, err
	}

	cmdStr, env := prepareCmdEnv(ctx)
	cmd := exec.CreateCmd(cmdStr, workDir)
	cmd.Env = env

	// Send the template on cozy-mjml stdin
	cmd.Stdin = bytes.NewReader(template)

	// Log out all things printed in stderr
	var stderrBuf bytes.Buffer
	cmd.Stderr = utils.LimitWriterDiscard(&stderrBuf, 256*1024)

	out, err := cmd.Output()
	if logErrors && stderrBuf.Len() > 0 {
		log.Errorf("Stderr: %s", stderrBuf.String())
	}
	if err != nil {
		if logErrors {
			log.Errorf("Run: %s", err)
		}
		return nil, err
	}

	return out, nil
}

func prepareWorkDir() (string, func(), error) {
	cleanDir := func() {}
	osFS := afero.NewOsFs()
	workDir, err := afero.TempDir(osFS, "", "mjml")
	if err != nil {
		return "", cleanDir, err
	}
	cleanDir = func() {
		_ = os.RemoveAll(workDir)
	}
	workFS := afero.NewBasePathFs(osFS, workDir)
	dst, err := workFS.OpenFile("index.js", os.O_CREATE|os.O_WRONLY, 0640)
	if err != nil {
		return "", cleanDir, err
	}
	f, err := assets.Open("/js/cozy-mjml.js", config.DefaultInstanceContext)
	if err != nil {
		return "", cleanDir, err
	}
	_, _ = io.Copy(dst, f)
	if err = dst.Close(); err != nil {
		return "", cleanDir, err
	}
	return workDir, cleanDir, err
}

func prepareCmdEnv(ctx *job.TaskContext) (string, []string) {
	cmd := config.GetConfig().Konnectors.Cmd
	env := []string{
		"COZY_URL=" + ctx.Instance.PageURL("/", nil),
		"COZY_LANGUAGE=node",
		"COZY_LOCALE=" + ctx.Instance.Locale,
		"COZY_TIME_LIMIT=60",
		"COZY_JOB_ID=" + ctx.ID(),
	}
	return cmd, env
}
