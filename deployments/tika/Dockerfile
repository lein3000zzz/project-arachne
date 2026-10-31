# Apache Tika for the documents labs add: PDFs, Office files, EPUB, scans.
# The full image carries Tesseract with English and a few western languages;
# Russian is added, and curl for the health check.
FROM apache/tika:4.2.0-full@sha256:4774eaac44f86cdf648ae7b5a367016eaf6f9052adec31e5cff07cff1a0c6e24
USER root
RUN apt-get update \
    && apt-get install -y --no-install-recommends tesseract-ocr-rus curl \
    && rm -rf /var/lib/apt/lists/*
COPY tika-config.json /tika-config.json
USER 35002:35002
CMD ["-c", "/tika-config.json"]
