# Changelog

## [0.2.0](https://github.com/cccteam/gh-jira-sync/compare/v0.1.3...v0.2.0) (2026-11-05)


### ⚠ BREAKING CHANGES

* project_status.jira_board and map[].jira_backlog are no longer accepted. Move the issues in the board's backlog into a Backlog status, then map that status like any other.

### Features

* remove board backlog mode; map a Jira Backlog status instead ([#21](https://github.com/cccteam/gh-jira-sync/issues/21)) ([f418de0](https://github.com/cccteam/gh-jira-sync/commit/f418de011c5db7059d98f7146773db8844248038))
* sync titles and descriptions, and accept Jira issue type names ([#16](https://github.com/cccteam/gh-jira-sync/issues/16)) ([cd49101](https://github.com/cccteam/gh-jira-sync/commit/cd49101c7a788cc699e6b8ba27138116635e098d))


### Bug Fixes

* name the service and API path in HTTP errors ([#13](https://github.com/cccteam/gh-jira-sync/issues/13)) ([f96ed3c](https://github.com/cccteam/gh-jira-sync/commit/f96ed3cdcf16b8a7b515296be99e5f9cc3d6c413))
* stop before transitioning when a board move does not take effect ([#14](https://github.com/cccteam/gh-jira-sync/issues/14)) ([457a001](https://github.com/cccteam/gh-jira-sync/commit/457a001bf9772dbc3d4547f28ab0e17a89a8db37))
* wait up to a minute for Jira's backlog listing to show a board move ([#19](https://github.com/cccteam/gh-jira-sync/issues/19)) ([35872ad](https://github.com/cccteam/gh-jira-sync/commit/35872ad8f303941366e3ebcd2b8f5cd3e5c24c2a))


### Performance Improvements

* read linked Jira issues eight at a time before planning ([#20](https://github.com/cccteam/gh-jira-sync/issues/20)) ([53cf4b1](https://github.com/cccteam/gh-jira-sync/commit/53cf4b11cebf5f7c6488bff49970f9e8ef645a10))


### Documentation

* bring the README up to date with sync_content and description rendering ([#18](https://github.com/cccteam/gh-jira-sync/issues/18)) ([caf7f6b](https://github.com/cccteam/gh-jira-sync/commit/caf7f6bb28966646ced6fd10295796cd1ffaba0b))

## [0.1.3](https://github.com/cccteam/gh-jira-sync/compare/v0.1.2...v0.1.3) (2026-10-30)


### Features

* support Atlassian service accounts with OAuth or API tokens ([#11](https://github.com/cccteam/gh-jira-sync/issues/11)) ([6478b44](https://github.com/cccteam/gh-jira-sync/commit/6478b444e44d5e86927f070ca906155bff320ca0))

## [0.1.2](https://github.com/cccteam/gh-jira-sync/compare/v0.1.1...v0.1.2) (2026-10-26)


### Bug Fixes

* stop failing hourly sync, bind release attestations to the tag, and simplify the README ([6dd9c13](https://github.com/cccteam/gh-jira-sync/commit/6dd9c135248dc95dd322e1e2397390fd761b43f1))
* stop failing hourly sync, verify release tag, simplify README ([#7](https://github.com/cccteam/gh-jira-sync/issues/7)) ([6dd9c13](https://github.com/cccteam/gh-jira-sync/commit/6dd9c135248dc95dd322e1e2397390fd761b43f1))


### Documentation

* add a project icon to the README ([#9](https://github.com/cccteam/gh-jira-sync/issues/9)) ([c447dbf](https://github.com/cccteam/gh-jira-sync/commit/c447dbf09ae33310567c06b272d08fa342cb59c9))

## [0.1.1](https://github.com/cccteam/gh-jira-sync/compare/v0.1.0...v0.1.1) (2026-10-26)


### Bug Fixes

* attach release binaries before publishing immutable releases ([#5](https://github.com/cccteam/gh-jira-sync/issues/5)) ([e58d6b1](https://github.com/cccteam/gh-jira-sync/commit/e58d6b194d61955f288648775287f4ed996f9c79))

## 0.1.0 (2026-10-26)


### Features

* versioned releases usable from any organization ([ee8f5c5](https://github.com/cccteam/gh-jira-sync/commit/ee8f5c5b0a0872ab7e8bdce532b9f45b8f2adccd))
* versioned releases usable from any organization ([488c469](https://github.com/cccteam/gh-jira-sync/commit/488c469840bb484eb481e64e164a60f996306d6b))
* versioned releases usable from any organization ([3fd9c83](https://github.com/cccteam/gh-jira-sync/commit/3fd9c83ec72a64ab71700c61b91a9132b414ba4c))


### Code Upgrade

* **deps:** bump actions/create-github-app-token from 3.2.0 to 3.3.0 in the github-actions group ([#4](https://github.com/cccteam/gh-jira-sync/issues/4)) ([231a4dc](https://github.com/cccteam/gh-jira-sync/commit/231a4dc7ccbec3513126e05b77b77b105b195016))
