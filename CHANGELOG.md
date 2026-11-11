# Changelog

## [0.2.8](https://github.com/cccteam/gh-jira-sync/compare/v0.2.7...v0.2.8) (2026-11-11)


### Bug Fixes

* accept Enterprise Managed User logins in assignee_map ([#38](https://github.com/cccteam/gh-jira-sync/issues/38)) ([065201d](https://github.com/cccteam/gh-jira-sync/commit/065201d6ab57306d439f0d7b6eb8dd6261b0d076))

## [0.2.7](https://github.com/cccteam/gh-jira-sync/compare/v0.2.6...v0.2.7) (2026-11-11)


### Features

* sync assignees for people listed in assignee_map ([#36](https://github.com/cccteam/gh-jira-sync/issues/36)) ([4273048](https://github.com/cccteam/gh-jira-sync/commit/42730486b6d0b0925048809ed4a4729f8f9e19e3))

## [0.2.6](https://github.com/cccteam/gh-jira-sync/compare/v0.2.5...v0.2.6) (2026-11-10)


### Bug Fixes

* keep the project status when a new Jira issue is created from GitHub ([#34](https://github.com/cccteam/gh-jira-sync/issues/34)) ([63f521f](https://github.com/cccteam/gh-jira-sync/commit/63f521f1132dc6099ce27e869a19cc0117bc941a))

## [0.2.5](https://github.com/cccteam/gh-jira-sync/compare/v0.2.4...v0.2.5) (2026-11-10)


### Features

* add a readable job summary, and keep sync runs going through stale items ([#32](https://github.com/cccteam/gh-jira-sync/issues/32)) ([ae97f3d](https://github.com/cccteam/gh-jira-sync/commit/ae97f3d071bacb25a482fd1ee70bc753484c0f1c))

## [0.2.4](https://github.com/cccteam/gh-jira-sync/compare/v0.2.3...v0.2.4) (2026-11-09)


### Features

* add Jira-owned issue types, ignored Jira parents, and an acknowledge workflow input ([#30](https://github.com/cccteam/gh-jira-sync/issues/30)) ([69c561d](https://github.com/cccteam/gh-jira-sync/commit/69c561dd5ef860003733604eda3173b50d5f2e4c))

## [0.2.3](https://github.com/cccteam/gh-jira-sync/compare/v0.2.2...v0.2.3) (2026-11-06)


### Features

* rename jira_issue_type to jira_default_issue_type, and explain every sync.json option ([#28](https://github.com/cccteam/gh-jira-sync/issues/28)) ([e0ca543](https://github.com/cccteam/gh-jira-sync/commit/e0ca54300bed53ca36ae2ae76fa4d7900d184139))

## [0.2.2](https://github.com/cccteam/gh-jira-sync/compare/v0.2.1...v0.2.2) (2026-11-05)


### Features

* settle project status conflicts for the side that changed last ([#26](https://github.com/cccteam/gh-jira-sync/issues/26)) ([8464ad2](https://github.com/cccteam/gh-jira-sync/commit/8464ad2d85a28e9be853adf2564055518e1437d4))

## [0.2.1](https://github.com/cccteam/gh-jira-sync/compare/v0.2.0...v0.2.1) (2026-11-05)


### Bug Fixes

* confirm table descriptions, and ignore project workflow echoes ([#22](https://github.com/cccteam/gh-jira-sync/issues/22)) ([a651a2f](https://github.com/cccteam/gh-jira-sync/commit/a651a2f01200ef9875a37a3393dda87ee70990e5))

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
