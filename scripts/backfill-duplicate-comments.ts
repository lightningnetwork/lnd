#!/usr/bin/env bun

declare global {
  var process: {
    env: Record<string, string | undefined>;
  };
}

interface GitHubIssue {
  number: number;
  title: string;
  state: string;
  state_reason?: string;
  user: { id: number };
  created_at: string;
  closed_at?: string;
}

interface GitHubComment {
  id: number;
  body: string;
  created_at: string;
  user: { type: string; id: number };
}

async function githubRequest<T>(endpoint: string, token: string, method: string = 'GET', body?: any): Promise<T> {
  const response = await fetch(`https://api.github.com${endpoint}`, {
    method,
    headers: {
      Authorization: `Bearer ${token}`,
      Accept: "application/vnd.github.v3+json",
      "User-Agent": "backfill-duplicate-comments-script",
      ...(body && { "Content-Type": "application/json" }),
    },
    ...(body && { body: JSON.stringify(body) }),
  });

  if (!response.ok) {
    throw new Error(
      `GitHub API request failed: ${response.status} ${response.statusText}`
    );
  }

  return response.json();
}

async function triggerDedupeWorkflow(
  owner: string,
  repo: string,
  issueNumber: number,
  token: string,
  dryRun: boolean = true
): Promise<void> {
  if (dryRun) {
    console.log(`[DRY RUN] Would trigger dedupe workflow for issue #${issueNumber}`);
    return;
  }

  await githubRequest(
    `/repos/${owner}/${repo}/actions/workflows/claude-dedupe-issues.yml/dispatches`,
    token,
    'POST',
    {
      ref: 'main',
      inputs: {
        issue_number: issueNumber.toString()
      }
    }
  );
}

function getCutoffDate(daysBack: number): Date {
  const cutoff = new Date();
  cutoff.setDate(cutoff.getDate() - daysBack);
  return cutoff;
}

async function backfillDuplicateComments(): Promise<void> {
  console.log("[DEBUG] Starting backfill duplicate comments script");

  const token = process.env.GITHUB_TOKEN;
  if (!token) {
    throw new Error(`GITHUB_TOKEN environment variable is required

Usage:
  GITHUB_TOKEN=your_token bun run scripts/backfill-duplicate-comments.ts

Environment Variables:
  GITHUB_TOKEN - GitHub personal access token with repo and actions permissions (required)
  DRY_RUN - Set to "false" to actually trigger workflows (default: true for safety)
  DAYS_BACK - Only process issues created within the last N days (default: 90)
  MIN_ISSUE_NUMBER - Only process issues with numbers >= this value (optional)
  MAX_ISSUE_NUMBER - Only process issues with numbers < this value (optional)`);
  }
  console.log("[DEBUG] GitHub token found");

  const owner = process.env.GITHUB_REPOSITORY_OWNER || "lightningnetwork";
  const repo = process.env.GITHUB_REPOSITORY_NAME || "lnd";
  const dryRun = process.env.DRY_RUN !== "false";
  const daysBack = parseInt(process.env.DAYS_BACK || "90", 10);
  const minIssueNumberEnv = process.env.MIN_ISSUE_NUMBER;
  const maxIssueNumberEnv = process.env.MAX_ISSUE_NUMBER;
  const minIssueNumber = minIssueNumberEnv ? parseInt(minIssueNumberEnv, 10) : undefined;
  const maxIssueNumber = maxIssueNumberEnv ? parseInt(maxIssueNumberEnv, 10) : undefined;
  const cutoffDate = getCutoffDate(daysBack);
  
  console.log(`[DEBUG] Repository: ${owner}/${repo}`);
  console.log(`[DEBUG] Dry run mode: ${dryRun}`);
  console.log(`[DEBUG] Looking back ${daysBack} days. Cutoff: ${cutoffDate.toISOString()}`);
  if (minIssueNumber !== undefined || maxIssueNumber !== undefined) {
    console.log(`[DEBUG] Issue number bounds: min=${minIssueNumber ?? 'none'} max=${maxIssueNumber ?? 'none'}`);
  }

  console.log("[DEBUG] Fetching issues... (sorted by created desc)");
  const allIssues: GitHubIssue[] = [];
  let page = 1;
  const perPage = 100;
  
  while (true) {
    const pageIssues: GitHubIssue[] = await githubRequest(
      `/repos/${owner}/${repo}/issues?state=all&per_page=${perPage}&page=${page}&sort=created&direction=desc`,
      token
    );
    
    if (pageIssues.length === 0) break;

    // Filter by date range
    const filteredByDate = pageIssues.filter(issue =>
      new Date(issue.created_at) >= cutoffDate
    );

    // Apply optional issue number bounds
    const filteredIssues = filteredByDate.filter(issue => {
      if (minIssueNumber !== undefined && issue.number < minIssueNumber) return false;
      if (maxIssueNumber !== undefined && issue.number >= maxIssueNumber) return false;
      return true;
    });

    allIssues.push(...filteredIssues);

    const oldestIssueInPage = pageIssues[pageIssues.length - 1];
    if (oldestIssueInPage && new Date(oldestIssueInPage.created_at) < cutoffDate) {
      console.log(`[DEBUG] Oldest issue in page #${page} is ${oldestIssueInPage.created_at}, below cutoff, stopping`);
      break;
    }

    page++;

    // Safety limit to avoid infinite loops
    if (page > 200) {
      console.log("[DEBUG] Reached page limit, stopping pagination");
      break;
    }
  }
  
  console.log(`[DEBUG] Found ${allIssues.length} issues in range`);

  let processedCount = 0;
  let candidateCount = 0;
  let triggeredCount = 0;

  for (const issue of allIssues) {
    processedCount++;
    console.log(
      `[DEBUG] Processing issue #${issue.number} (${processedCount}/${allIssues.length}): ${issue.title}`
    );

    console.log(`[DEBUG] Fetching comments for issue #${issue.number}...`);
    const comments: GitHubComment[] = await githubRequest(
      `/repos/${owner}/${repo}/issues/${issue.number}/comments`,
      token
    );
    console.log(
      `[DEBUG] Issue #${issue.number} has ${comments.length} comments`
    );

    // Look for existing duplicate detection comments (from the dedupe bot)
    const dupeDetectionComments = comments.filter(
      (comment) =>
        comment.body.includes("Found") &&
        comment.body.includes("possible duplicate") &&
        comment.user.type === "Bot"
    );

    console.log(
      `[DEBUG] Issue #${issue.number} has ${dupeDetectionComments.length} duplicate detection comments`
    );

    // Skip if there's already a duplicate detection comment
    if (dupeDetectionComments.length > 0) {
      console.log(
        `[DEBUG] Issue #${issue.number} already has duplicate detection comment, skipping`
      );
      continue;
    }

    candidateCount++;
    const issueUrl = `https://github.com/${owner}/${repo}/issues/${issue.number}`;
    
    try {
      console.log(
        `[INFO] ${dryRun ? '[DRY RUN] ' : ''}Triggering dedupe workflow for issue #${issue.number}: ${issueUrl}`
      );
      await triggerDedupeWorkflow(owner, repo, issue.number, token, dryRun);
      
      if (!dryRun) {
        console.log(
          `[SUCCESS] Successfully triggered dedupe workflow for issue #${issue.number}`
        );
      }
      triggeredCount++;
    } catch (error) {
      console.error(
        `[ERROR] Failed to trigger workflow for issue #${issue.number}: ${error}`
      );
    }

    // Add a delay between workflow triggers to avoid overwhelming the system
    await new Promise(resolve => setTimeout(resolve, 1000));
  }

  console.log(
    `[DEBUG] Script completed. Processed ${processedCount} issues, found ${candidateCount} candidates without duplicate comments, ${dryRun ? 'would trigger' : 'triggered'} ${triggeredCount} workflows`
  );
}

backfillDuplicateComments().catch(console.error);

// Make it a module
export {};
