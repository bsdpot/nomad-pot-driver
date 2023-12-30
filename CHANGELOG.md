# Changelog
All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [0.10.0] 2023-12-30
### Fixed
- Fix paths in repo to point to bsdpot (#50)
- Fix pot-stop and pot-destroy command invocations (#49)
- Use shellescape in more potentially dangerous places (#44)

## [0.9.1] 2023-09-29
### Added
- Add optional keyword "attributes" to set pot attributes like `devfs_ruleset` on the task (#42)
- Escape environment variables set on pot - this might break existing workarounds in jobs (#43)

## [0.9.0] 2022-09-11
### Added
- Added changelog
- Improve batch job and restart behavior (#30)
- Add support for signals (#31)
- Add support for exec (#32)

## [0.6.0] 2020-02-17
