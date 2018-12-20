#### Pull Request Checklist

- [ ] If this is your first time contributing, we recommend you read the [Code
  Contribution Guidelines](https://github.com/lightningnetwork/lnd/blob/master/docs/code_contribution_guidelines.md)
- [ ] All changes are Go version 1.11 compliant
- [ ]  The code being submitted is commented according to the
  [Code Documentation and Commenting](#CodeDocumentation) section
- [ ]  For new code: Code is accompanied by tests which exercise both
  the positive and negative (error paths) conditions (if applicable)
- [ ]  For bug fixes: Code is accompanied by new tests which trigger
  the bug being fixed to prevent regressions
- [ ]  Any new logging statements use an appropriate subsystem and
  logging level
- [ ]  Code has been formatted with `go fmt`
- [ ]  For code and documentation: lines are wrapped at 80 characters
  (the tab character should be counted as 8 characters, not 4, as some IDEs do
  per default)
- [ ]  Running `make check` does not fail any tests
- [ ]  Running `go vet` does not report any issues
- [ ]  Running `make lint` does not report any **new** issues that did not
  already exist
- [ ] All commits build properly and pass tests. Only in exceptional
  cases it can be justifiable to violate this condition. In that case, the
  reason should be stated in the commit message.
- [ ] Commits have a logical structure ([see section 4.5, of the Code Contribution Guidelines])
