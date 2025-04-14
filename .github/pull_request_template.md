## Change Description
Description of change / link to associated issue.

## Steps to Test
Steps for reviewers to follow to test the change.

## Pull Request Checklist
### Testing
- [ ] Your PR passes all CI checks.
- [ ] Tests covering the positive and negative (error paths) are included.
- [ ] Bug fixes contain tests triggering the bug to prevent regressions.

### Code Style and Documentation
- [ ] The change is not [insubstantial](https://github.com/lightningnetwork/lnd/blob/master/docs/code_contribution_guidelines.md#substantial-contributions-only). Typo fixes are not accepted to fight bot spam.
- [ ] The change obeys the [Code Documentation and Commenting](https://github.com/lightningnetwork/lnd/blob/master/docs/development_guidelines.md#code-documentation-and-commenting) guidelines, and lines wrap at 80.
- [ ] Commits follow the [Ideal Git Commit Structure](https://github.com/lightningnetwork/lnd/blob/master/docs/development_guidelines.md#ideal-git-commit-structure).
- [ ] Any new logging statements use an appropriate subsystem and logging level.
- [ ] Any new lncli commands have appropriate tags in the comments for the rpc in the proto file.
- [ ] [There is a change description in the release notes](https://github.com/lightningnetwork/lnd/tree/master/docs/release-notes), or `[skip ci]` in the commit message for small changes.

📝 Please see our [Contribution Guidelines](https://github.com/lightningnetwork/lnd/blob/master/docs/code_contribution_guidelines.md) for further guidance.
