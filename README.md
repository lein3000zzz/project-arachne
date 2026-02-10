<div align="center">
    <img src="https://github.com/lein3000zzz/project-arachne/blob/main/assets/logoTransparent.png?raw=true" height=400 width=400 alt="🦍">
</div>

# Project arachne 🕷

## Description

A high-performance, distributed web crawler built in Go that leverages browser automation to crawl and parse dynamic web content. 
The crawler supports depth-limited crawling, handles JavaScript-rendered pages, respects robots.txt, and stores crawled data in a graph database for efficient querying and analysis.

## Features

- **Browser Automation**: Uses Rod (Go wrapper for Puppeteer) to render JavaScript-heavy websites and extract dynamic content.
- **Multi-Format Parsing**: Extracts links from HTML, JavaScript, and JSON responses.
  - js parsing relies heavily on [goja](https://github.com/dop251/goja)
- **Depth-Limited Crawling**: Configurable maximum depth and link limits per crawl run.
- **Robots.txt Compliance**: Automatically checks and respects robots.txt files.
- **Caching**: Redis-based caching for pages and robots.txt to improve performance and reduce redundant requests.
- **Graph Database Storage**: Stores crawled pages and their relationships in Neo4j for advanced querying.
- **Message Queue**: Uses Apache Kafka for distributed task processing and run management.
- **Screenshot Capture**: Optional screenshot functionality for visual page archiving.
- **Concurrent Processing**: Configurable number of concurrent workers for efficient crawling.
- **Dockerized Deployment**: Complete containerized setup with Docker Compose.

## Architecture

The crawler consists of several key components:

- **Crawler**: Main crawling logic using browser automation
- **Processor**: Kafka-based message processing for tasks and runs
- **Page Parser**: Extracts links from various content types
- **Networker**: Handles HTTP requests and browser interactions
- **Page Repository**: Manages data persistence in Neo4j
- **Cache**: Redis-based caching layer

### Data Flow

1. Run configurations are sent to Kafka
2. Processor creates initial tasks
3. Crawler workers process tasks concurrently
4. Pages are parsed for links and stored in Neo4j
5. New tasks are generated for discovered links (within depth limits)

## License

This project is licensed under the MIT License - see the LICENSE file for details.