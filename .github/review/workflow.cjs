const assert = require('node:assert/strict')
const { execFileSync } = require('node:child_process')
const { mkdirSync, readFileSync, writeFileSync } = require('node:fs')

const directions = ['security', 'correctness', 'simplicity', 'alternatives', 'tests-performance']
const promptDir = '.github/review'

function checkPublicPR(pr) {
  assert(pr.base.repo.private === false && pr.head.repo?.private === false,
    'This gateway setup only accepts public repositories and public PR branches')
  assert(pr.state === 'open' && !pr.draft, 'PR is closed or draft')
}

async function gate({ github, context, core }) {
  if (context.eventName !== 'pull_request_target') {
    const { data } = await github.rest.repos.getCollaboratorPermissionLevel({
      ...context.repo, username: context.actor
    })
    assert(['admin', 'maintain', 'write'].includes(data.permission), 'Write access is required')
  }
  const number = Number(context.payload.pull_request?.number || context.payload.issue?.number ||
    context.payload.inputs?.pr_number)
  assert(Number.isSafeInteger(number) && number > 0, 'Invalid PR number')
  const { data: pr } = await github.rest.pulls.get({ ...context.repo, pull_number: number })
  if (pr.state !== 'open' || pr.draft) return
  checkPublicPR(pr)
  if (context.eventName === 'pull_request') {
    assert(pr.head.sha === context.payload.pull_request.head.sha,
      'Trial commit was superseded; use the run for the latest push')
  }
  core.setOutput('snapshot', JSON.stringify({ number, head: pr.head.sha, base: pr.base.sha }))
  core.setOutput('pr', number)
  core.setOutput('url', pr.html_url)
  core.setOutput('prompt_sha', execFileSync('git', ['rev-parse', 'HEAD'], { encoding: 'utf8' }).trim())
  core.setOutput('ready', 'true')
}

async function checkRevision({ github, context }) {
  const snapshot = JSON.parse(process.env.REVIEW_SNAPSHOT)
  const { data: pr } = await github.rest.pulls.get({ ...context.repo, pull_number: snapshot.number })
  checkPublicPR(pr)
  assert(pr.head.sha === snapshot.head && pr.base.sha === snapshot.base,
    'PR changed during review; rerun on the latest revision')
}

function readResult(path) {
  const result = JSON.parse(readFileSync(path, 'utf8'))
  assert(result.review?.status === 'complete', `${path}: review is incomplete; inspect the artifact`)
  assert(typeof result.review.summary === 'string' && typeof result.review.limitations === 'string',
    `${path}: missing review summary or limitations`)
  assert(Array.isArray(result.review.key_issues_to_review), `${path}: missing findings`)
  for (const finding of result.review.key_issues_to_review) {
    for (const field of ['relevant_file', 'issue_header', 'issue_content']) {
      assert(typeof finding?.[field] === 'string' && finding[field].trim(),
        `${path}: invalid finding ${field}`)
    }
    assert(Number.isSafeInteger(finding.start_line) && finding.start_line > 0 &&
      Number.isSafeInteger(finding.end_line) && finding.end_line >= finding.start_line,
    `${path}: invalid finding line range`)
  }
  for (const key of ['files_not_reviewed', 'files_partially_reviewed']) {
    assert(Array.isArray(result.coverage?.[key]) && result.coverage[key].length === 0,
      `${path}: PR-Agent omitted or clipped files; split the PR or review it manually`)
  }
  return result
}

async function prepare(api, direction) {
  assert([...directions, 'verify'].includes(direction), 'Unknown review direction')
  await checkRevision(api)
  mkdirSync('review-results', { recursive: true })
  const system = ['common', direction, 'report'].map(name =>
    readFileSync(`${promptDir}/${name}.md`, 'utf8')).join('\n\n')
  api.core.exportVariable('PR_REVIEW_PROMPT__SYSTEM', system)
  api.core.exportVariable('PR_REVIEW_PROMPT__USER', readFileSync(`${promptDir}/context.md`, 'utf8'))
  if (direction === 'verify') {
    const candidates = Object.fromEntries(directions.map(name =>
      [name, readResult(`review-results/${name}.json`)]))
    const content = JSON.stringify(candidates)
    assert(content.length <= 200000, 'Review candidates exceed the artifact context limit')
    writeFileSync('review-results/candidates.json', content)
    api.core.exportVariable('ARTIFACTS__ENABLE', 'true')
    api.core.exportVariable('ARTIFACTS__ARTIFACT_PATH', 'review-results/candidates.json')
    api.core.exportVariable('ARTIFACTS__MAX_ARTIFACT_SIZE', '200000')
  }
}

async function finish(api, direction) {
  // shortcut: late pushes are detected after publication; separate publishing if atomic checks become necessary.
  await checkRevision(api)
  readResult(`review-results/${direction}.json`)
}

module.exports = { directions, checkPublicPR, gate, checkRevision, readResult, prepare, finish }
