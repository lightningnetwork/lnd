#### Pull Request Checklist

- [ ] All changes are Go version 1.16 compliant
- [ ] Your PR passes all CI checks. If a check cannot be passed for a justifiable reason, that reason must be stated in the commit message and PR description.
- [ ] If this is your first time contributing, we recommend you read the [Code Contribution Guidelines](https://github.com/lightningnetwork/lnd/blob/master/docs/code_contribution_guidelines.md)
   - [ ] The code being submitted is commented according to [Code Documentation and Commenting](https://github.com/lightningnetwork/lnd/blob/master/docs/code_contribution_guidelines.md#CodeDocumentation)
   - [ ] Commits have a logical structure according to [Ideal Git Commit Structure](https://github.com/lightningnetwork/lnd/blob/master/docs/code_contribution_guidelines.md#IdealGitCommitStructure)
- [ ] For new code: Code is accompanied by tests which exercise both the positive and negative (error paths) conditions (if applicable)
- [ ] For bug fixes: If possible, code is accompanied by new tests which trigger the bug being fixed to prevent regressions
- [ ] Any new logging statements use an appropriate subsystem and logging level
- [ ] For code and documentation: lines are wrapped at 80 characters (the tab character should be counted as 8 characters, not 4, as some IDEs do per default)
- [ ] A description of your changes [should be added to running the release notes](https://github.com/lightningnetwork/lnd/tree/master/docs/release-notes) for the milestone your change will land in.
