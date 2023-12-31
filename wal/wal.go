package wal

import (
	"FlexDB/fio"
	"encoding/binary"
	"github.com/hashicorp/golang-lru/v2"
	"hash/crc32"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Wal struct {
	currBlcokOffset uint32                     //当前指向的block中的偏移大小
	currSegOffset   uint32                     //当前segment文件中的偏移量
	segmentID       uint32                     //当前指向的文件ID
	BlockId         uint32                     //当前处理到的blockId
	activeFile      *Segment                   //当前指向的活跃的segment文件
	olderFile       map[uint32]*Segment        //当前文件已经达到阈值之后就开辟一个新的文件来进行处理
	mu              *sync.RWMutex              //当前Wal持有的读写锁
	option          WalOption                  //当前的wal的配置项
	cache           *lru.Cache[uint32, []byte] //缓存block数据,key是blockId，value是一个block大小的缓存
	isEmpty         bool                       //是否为空文件,如果当前为空文件，就不能进行读取操作
}

type WalOption struct {
	DirPath            string //所在的路经名
	BlockSize          uint32 //一个block固定是32KB
	SegmentMaxBlockNum uint32 //一个segment文件中最多可以存放多少个Block
	SegmentSize        uint32 //一个segment文件最大可以最大的大小
	BlockCacheNum      int    //lru中可以缓存多少个Block节点
	FileSuffix         string //文件的后缀名
}

var DefaultWalOpt = WalOption{
	DirPath:            "/home/zevin/tmp",
	BlockSize:          32 * 1024,
	SegmentMaxBlockNum: 1024,
	SegmentSize:        32 * 1024 * 1024,
	BlockCacheNum:      20,
	FileSuffix:         ".seg",
}

//Open 打开一个Wal实例
func Open(options WalOption) (*Wal, error) {
	//检查当前目录是否存在，如果不存在的话就需要创建
	if _, err := os.Stat(options.DirPath); os.IsNotExist(err) {
		//创建目录
		if err := os.MkdirAll(options.DirPath, os.ModePerm); err != nil {
			return nil, err
		}
	}
	wal := &Wal{
		mu:        new(sync.RWMutex),
		olderFile: make(map[uint32]*Segment),
		option:    options,
		isEmpty:   true,
	}
	//设置LRU缓存
	if err := wal.setCache(); err != nil {
		return nil, err
	}
	//加载当前目录的所有数据文件
	fileIds, err := wal.loadFiles()
	if err != nil {
		return nil, err
	}
	//遍历每个文件ID，打开对应的文件
	segNum, err := wal.openFiles(fileIds)
	if err != nil {
		return nil, err
	}
	//更新wal中的数据
	blockID := uint32(segNum) * wal.option.SegmentMaxBlockNum
	if wal.activeFile != nil {
		activeSize, err := wal.activeFile.Size()
		if err != nil {
			return nil, err
		}
		wal.currSegOffset = activeSize
		//blockIdInCurrSeg:=activeSize/BlockSize
		wal.currBlcokOffset = activeSize % wal.option.BlockSize

		blockID = blockID + activeSize/wal.option.BlockSize //这个问题
		wal.BlockId = blockID
		wal.isEmpty = false
	}

	return wal, nil
}

//setCache 设置缓存
func (wal *Wal) setCache() error {
	if wal.option.BlockCacheNum > 0 {
		cache, err := lru.New[uint32, []byte](wal.option.BlockCacheNum)
		if err != nil {
			return err
		}
		wal.cache = cache
	}
	return nil
}

//loadFiles 加载数据文件
func (wal *Wal) loadFiles() ([]int, error) {
	//读取当前目录下的所有.seg文件
	dirEntries, err := os.ReadDir(wal.option.DirPath)
	if err != nil {
		return nil, err
	}
	var fileIds []int
	//遍历目录中的所有文件,找到所有以.data结尾的文件
	for _, entry := range dirEntries {
		if strings.HasSuffix(entry.Name(), wal.option.FileSuffix) {
			//对00001.data文件进行分割，拿到他的第一个部分00001

			trimmedName := strings.TrimLeft(entry.Name()[:len(entry.Name())-len(wal.option.FileSuffix)], "0") //去掉前导0
			// 转换为文件ID
			if trimmedName == "" {
				trimmedName = "0"
			}
			//获得文件ID
			fileId, err := strconv.Atoi(trimmedName) //获得文件ID
			if err != nil {
				return nil, err
			}
			fileIds = append(fileIds, fileId)
		}
	}
	//对文件ID进行排序，从小到大
	sort.Ints(fileIds)
	return fileIds, nil
}

//openFiles 打开所有的segment文件
func (wal *Wal) openFiles(fileIds []int) (int, error) {
	//遍历每个文件ID，打开对应的文件
	var segNum = 0
	for i, fid := range fileIds {
		segFile, err := wal.OpenSegment(uint32(fid), wal.option, fio.MMapFio)
		if err != nil {
			return 0, err
		}
		if i == len(fileIds)-1 {
			//说明这个是最后一个id，就设置成活跃文件
			wal.activeFile = segFile
			wal.segmentID = uint32(fid)
			err := wal.activeFile.SetIOManager(wal.option.DirPath, wal.option.FileSuffix, fio.StanderFIO)
			if err != nil {
				return 0, err
			} //设置成标准IO
		} else {
			//否则就放入到旧文件集合中
			wal.olderFile[uint32(fid)] = segFile
		}
		segNum = i
	}
	return segNum, nil
}

//closeFiles 关闭掉所有的文件
func (wal *Wal) closeFiles() error {
	if wal.activeFile == nil {
		return nil
	}
	if err := wal.activeFile.Sync(); err != nil {
		return err
	}
	if err := wal.activeFile.Close(); err != nil {
		return err
	}
	for _, file := range wal.olderFile {
		if err := file.Close(); err != nil {
			return err
		}
	}
	return nil
}

//Write 写入一个buf数据,并且返回具体写入的位置信息
func (wal *Wal) Write(data []byte) (*ChunkPos, error) {
	wal.mu.Lock()
	defer wal.mu.Unlock()
	if wal.activeFile == nil {
		//当前没有active文件，就需要新创建一个
		segfile, err := wal.OpenSegment(wal.segmentID, wal.option, fio.StanderFIO)
		if err != nil {
			return nil, nil
		}
		wal.activeFile = segfile
	}
	length := len(data) //获得当前数据的长度

	//data的数据不能超过一个segmentSize大小，超过的话，直接报错
	if uint32(length) >= wal.option.SegmentSize {
		return nil, ErrPayloadExceedSeg
	}
	blockFullWarning := headerSize+wal.currBlcokOffset >= wal.option.BlockSize //当前block无法容纳下一个heaeder
	if blockFullWarning {
		//填充占位字符
		wal.writePadding()
	}

	pos := &ChunkPos{
		segmentID:   wal.segmentID,
		blockID:     wal.BlockId,
		chunkOffset: wal.currBlcokOffset,
	}
	var blockWritable bool = uint32(length)+headerSize+wal.currBlcokOffset <= wal.option.BlockSize //当前的block是否可以被写入
	if blockWritable {
		//如果当前数据长度+头部数据+当前block中的偏移量小于一个block大小，就可以直接放进去
		//把数据编码，并写入
		chunkSize, err := wal.writeChunk(data, Full)
		if err != nil {
			return nil, err
		}
		pos.chunkSize = chunkSize
		wal.isEmpty = false

		return pos, nil
	}
	//如果走到这，说明当前的block无法容纳下该data，说明就需要将当前的data分在多个block中间存储

	var (
		begin        uint32    = 0 //两个指针指向要截取的数据的位置信息,begin指向的是当前的data读取的起点
		end          uint32    = uint32(length)
		chunkType    ChunkType //当前chunk的类型
		bytesToWrite uint32    //当前写入了多少字节的大小
	)
	//times := 0 //循环了多少次，多进行一次循环就多7字节
	for begin < end {
		if wal.currSegOffset+headerSize >= wal.option.SegmentSize {
			//如果当前文件剩余的空间连头部数据都写不进去，就需要新开辟一个文件，因为数据最多不会超过一个文件的大小，所以这边检查一下文件大小
			//将数据进行持久化到磁盘中
			//如果是因为文件满了，就不需要添加padding数据
			if err := wal.Sync(); err != nil {
				return nil, err
			}
			//设置进旧文件集合中
			wal.olderFile[wal.segmentID] = wal.activeFile
			//新打开一个segment文件
			err := wal.activeFile.SetIOManager(wal.option.DirPath, wal.option.FileSuffix, fio.MMapFio)
			if err != nil {
				return nil, err
			} //该文件达到阈值了，就设置成MMapIO

			wal.segmentID += 1
			segfile, err := wal.OpenSegment(wal.segmentID, wal.option, fio.StanderFIO)
			if err != nil {
				return nil, nil
			}
			wal.activeFile = segfile
			wal.currSegOffset = 0   //把当前segment文件的指针设置成0
			wal.currBlcokOffset = 0 //把当前block偏移置为0
		}
		if begin == 0 {
			// This is the first chunk
			chunkType = First
			bytesToWrite = wal.option.BlockSize - wal.currBlcokOffset - headerSize
		} else if end-begin+headerSize >= wal.option.BlockSize {
			// This is a middle chunk
			chunkType = Middle
			bytesToWrite = wal.option.BlockSize - headerSize
		} else {
			// This is the last chunk
			chunkType = Last
			bytesToWrite = end - begin
		}
		chunkSize, err := wal.writeChunk(data[begin:begin+bytesToWrite], chunkType)
		if err != nil {
			return nil, err
		}
		pos.chunkSize += chunkSize
		begin += bytesToWrite
	}
	wal.isEmpty = false
	return pos, nil
}

// WriteChunk 写入一个chunk数据
//返回chunk的大小
func (wal *Wal) writeChunk(data []byte, chunkType ChunkType) (uint32, error) {
	encBuf := encode(data, chunkType)
	wal.activeFile.append(encBuf)
	wal.BlockId = wal.BlockId + (wal.currBlcokOffset+uint32(len(encBuf)))/wal.option.BlockSize
	wal.currBlcokOffset = (wal.currBlcokOffset + uint32(len(encBuf))) % wal.option.BlockSize
	wal.currSegOffset = wal.currSegOffset + uint32(len(encBuf))
	return uint32(len(encBuf)), nil
}

//writePadding Block已经不够写了，写一个占位的字符
func (wal *Wal) writePadding() {
	buf := make([]byte, wal.option.BlockSize-wal.currBlcokOffset)
	wal.activeFile.append(buf)
	wal.BlockId++
	byteAdd := wal.option.BlockSize - wal.currBlcokOffset
	wal.currSegOffset += byteAdd
	wal.currBlcokOffset = 0
}

//Read 根据Pos位置来读取数据
//读取完pos开始的一系列有效数据之后，返回下一个可以开始读取的chunk的位置信息
func (wal *Wal) Read(pos *ChunkPos) ([]byte, *ChunkPos, error) {
	wal.mu.RLock()
	defer wal.mu.RUnlock()
	if wal.isEmpty {
		//如果为空，就不能读取，返回一个消息
		return nil, nil, ErrEmpty
	}

	if pos.segmentID > wal.segmentID || pos.blockID > wal.BlockId {
		return nil, nil, ErrPosNotValid
	}
	var segFile *Segment
	if pos.segmentID == wal.segmentID {
		//说明当前数据是在active中中
		segFile = wal.activeFile
	} else {
		//数据在old文件中
		segFile = wal.olderFile[pos.segmentID]
	}
	var (
		ret            []byte //返回的总数据长度
		blockId        = pos.blockID
		chunkOffset    = pos.chunkOffset
		nextChunkPos   = &ChunkPos{segmentID: pos.segmentID}
		segmentId      = pos.segmentID
		singleDataNum  uint32 //单次读取block获得有效数据的长度
		preBlockId     = pos.blockID
		preChunkOffset = pos.chunkOffset
	)

	for {

		isComplete, numBlockRead, data, err := segFile.ReadInternal(blockId, chunkOffset)
		if err != nil {
			return nil, nil, err
		}
		ret = append(ret, data...)
		singleDataNum += uint32(len(data)) + headerSize*numBlockRead
		//data = nil
		if isComplete {
			//当前的segment文件完全可以将全部数据读取上来
			break
		} else {
			//当前的数据无法在一个segment文件中全部读取上来,需要新开一个文件
			segmentId++
			if segmentId == wal.segmentID {
				//说明当前数据是在active中中
				segFile = wal.activeFile
			} else {
				//数据在old文件中
				segFile = wal.olderFile[segmentId]
			}
			blockId += numBlockRead //更新需要读取到哪个block中

			chunkOffset = 0
		}
	}

	nextChunkPos.segmentID = segmentId //更新下一次要读取数据所在的segment文件是哪一个
	nextChunkPos.chunkOffset = (preChunkOffset + singleDataNum) % wal.option.BlockSize
	nextChunkPos.blockID = preBlockId + (preChunkOffset+singleDataNum)/wal.option.BlockSize //更新下一个chunk读取的block的id是哪一个

	if nextChunkPos.chunkOffset+headerSize >= wal.option.BlockSize {
		//如果当前的需要开始读取的block小于一个header的大小
		//nextChunkPos.chunkOffset %= wal.option.BlockSize
		nextChunkPos.chunkOffset = 0 //被padding填充了，所以直接从下一个block开始
		nextChunkPos.blockID++
		if (nextChunkPos.segmentID+1)*wal.option.SegmentMaxBlockNum == nextChunkPos.blockID {
			//更新segmentId
			nextChunkPos.segmentID++
		}
	}

	return ret, nextChunkPos, nil
}

//Sync 将当前的活跃文件进行持久化
func (wal *Wal) Sync() error {
	if wal.activeFile == nil {
		return nil
	}
	return wal.activeFile.Sync()

}

//Close 关闭wal文件
func (wal *Wal) Close() error {
	//关闭掉所有的文件
	if err := wal.closeFiles(); err != nil {
		return err
	}
	return nil
}

//encode 将数据进行编码,编码出一个chunk出来
//Chunk的格式
//CRC     +     length    +   type   +   payload
//4       +       2       +    1     +     n
func encode(data []byte, chunkType ChunkType) []byte {
	encBuf := make([]byte, headerSize+len(data)) //开辟要返回的字节数组出来，返回
	//写入长度
	encBuf[6] = chunkType
	binary.LittleEndian.PutUint16(encBuf[4:], uint16(len(data))) //写入对应的data大小
	copy(encBuf[7:], data)
	//计算校验值
	crc := crc32.ChecksumIEEE(encBuf[4:])
	binary.LittleEndian.PutUint32(encBuf[:4], uint32(crc))
	return encBuf
}

// GetAllChunkInfo 获得所有的chunkPos的信息
func (wal *Wal) GetAllChunkInfo() ([][]byte, []*ChunkPos, error) {
	wal.mu.RLock()
	defer wal.mu.RUnlock()
	//如果当前为空Wal，也不允许进行读操作
	if wal.isEmpty {
		return nil, nil, ErrEmpty
	}
	var chunkPosArray []*ChunkPos
	var chunkPos = &ChunkPos{
		segmentID:   0,
		blockID:     0,
		chunkOffset: 0,
	}
	var res [][]byte
	for {
		data, nextchunkPos, err := wal.Read(chunkPos.Clone())
		if err != nil {
			//文件读取完了
			if err == io.EOF || err == ErrPosNotValid {
				break
			} else {
				return nil, nil, err
			}
		}
		res = append(res, data)
		chunkPosArray = append(chunkPosArray, chunkPos.Clone())
		chunkPos = nextchunkPos //更新下一次要开始的chunk的位置
		//走到这个位置之后，就无法继续往下面读取了
	}
	return res, chunkPosArray, nil
}

func (c *ChunkPos) Clone() *ChunkPos {
	return &ChunkPos{
		segmentID:   c.segmentID,
		blockID:     c.blockID,
		chunkOffset: c.chunkOffset,
	}
}
